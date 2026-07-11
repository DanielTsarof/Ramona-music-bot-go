package music

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"RamonaGo/modules/music/youtube"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

type Track struct {
	Input      string // local path or direct stream URL
	IsURL      bool
	Title      string
	Query      string       // original user query/URL — used to re-resolve a stale stream URL
	ChannelID  snowflake.ID // text channel to report playback errors to (0 = log only)
	WebpageURL string       // human-facing page URL (embed title link)
	Duration   int          // seconds, 0 = unknown
}

type Player struct {
	client  *bot.Client
	guildID snowflake.ID

	mu            sync.Mutex
	joinMu        sync.Mutex // serializes concurrent Join() calls
	conn          voice.Conn
	channelID     snowflake.ID // stored for reconnect
	currentCancel context.CancelFunc
	currentTrack  *Track

	queue    []Track
	notifyCh chan struct{}
	maxSize  int

	resumeCh chan struct{}
	paused   atomic.Bool
	looping  atomic.Bool

	idleTimeout time.Duration // 0 = never auto-leave on idle
	lastTextCh  snowflake.ID  // last text channel a track was queued from

	panelChannelID snowflake.ID // where the interactive player panel lives
	panelMessageID snowflake.ID
}

func NewPlayer(client *bot.Client, guildID snowflake.ID, maxSize int, idleTimeout time.Duration) *Player {
	if maxSize <= 0 {
		maxSize = 50
	}
	return &Player{
		client:      client,
		guildID:     guildID,
		notifyCh:    make(chan struct{}, 1),
		maxSize:     maxSize,
		resumeCh:    make(chan struct{}, 1),
		idleTimeout: idleTimeout,
	}
}

func (p *Player) Join(channelID snowflake.ID) error {
	p.joinMu.Lock()
	defer p.joinMu.Unlock()

	// If VoiceManager already has a Ready conn, reuse it instead of reconnecting.
	// This handles the case where sendWithReconnect just finished a successful join
	// and a concurrent handlePlay call arrives.
	if existing := p.client.VoiceManager.GetConn(p.guildID); existing != nil {
		if existing.Gateway().Status() == voice.StatusReady {
			p.mu.Lock()
			p.conn = existing
			p.channelID = channelID
			p.mu.Unlock()
			return nil
		}
		p.client.VoiceManager.RemoveConn(p.guildID)
	}

	// The first DAVE/MLS handshake tends to stall (not merely run slow) while a
	// fresh attempt right after succeeds quickly — so cut the first attempt
	// short and give the retry the full window.
	timeouts := []time.Duration{30 * time.Second, 60 * time.Second}
	var openErr error
	for attempt, timeout := range timeouts {
		if openErr = p.openConn(channelID, timeout); openErr == nil {
			return nil
		}
		log.Printf("voice join attempt %d/%d: %v", attempt+1, len(timeouts), openErr)
		if attempt < len(timeouts)-1 {
			time.Sleep(time.Second)
		}
	}
	return fmt.Errorf("voice join: %w", openErr)
}

// openConn creates a fresh voice conn and blocks until it is fully open.
//
// conn.Open blocks until SessionDescription is received (after the full DAVE
// MLS handshake). Waiting for it is the only way to guarantee that u.conn
// (set by udp.Open on OpcodeReady) and u.encrypter (set by SetSecretKey on
// SessionDescription) are both non-nil before we attempt any UDP writes.
// Polling StatusReady is insufficient: the gateway sets that flag one step
// before udp.Open runs, so returning early causes nil-pointer panics in
// safeUDPWrite → sendWithReconnect cascade. DAVE normally takes 20–30 s.
func (p *Player) openConn(channelID snowflake.ID, timeout time.Duration) error {
	conn := p.client.VoiceManager.CreateConn(p.guildID)

	openCtx, openCancel := context.WithTimeout(context.Background(), timeout)
	defer openCancel()

	if err := conn.Open(openCtx, channelID, false, false); err != nil {
		leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer leaveCancel()
		safeConnClose(conn, leaveCtx)
		p.client.VoiceManager.RemoveConn(p.guildID)
		return err
	}

	p.mu.Lock()
	p.conn = conn
	p.channelID = channelID
	p.mu.Unlock()
	return nil
}

func (p *Player) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil
}

// VoiceChannelID returns the voice channel the player is connected to (0 if none).
func (p *Player) VoiceChannelID() snowflake.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return 0
	}
	return p.channelID
}

// ResetVoiceState drops local voice state and clears the queue. Used when the
// bot was removed from the channel externally (kick/move) and the conn is dead.
func (p *Player) ResetVoiceState() {
	p.Stop()
	p.mu.Lock()
	p.conn = nil
	p.channelID = 0
	p.mu.Unlock()
}

func (p *Player) Disconnect() error {
	p.Stop()
	p.mu.Lock()
	conn := p.conn
	p.conn = nil
	p.channelID = 0
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if conn != nil {
		safeConnClose(conn, ctx) // sends VoiceStateUpdate(leave); removeConnFunc cleans VoiceManager
		return nil
	}
	// p.conn was nil (e.g. Join timed out before setting it, or sendWithReconnect cleared it).
	// Check VoiceManager for a stale conn so we still leave the channel.
	if existing := p.client.VoiceManager.GetConn(p.guildID); existing != nil {
		safeConnClose(existing, ctx)
	}
	return nil
}

func (p *Player) Enqueue(t Track) error {
	p.mu.Lock()
	if len(p.queue) >= p.maxSize {
		p.mu.Unlock()
		return errors.New("queue is full")
	}
	p.queue = append(p.queue, t)
	p.mu.Unlock()

	select {
	case p.notifyCh <- struct{}{}:
	default:
	}
	return nil
}

func (p *Player) dequeue() (Track, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return Track{}, false
	}
	t := p.queue[0]
	p.queue = p.queue[1:]
	return t, true
}

func (p *Player) ClearQueue() {
	p.mu.Lock()
	p.queue = nil
	p.mu.Unlock()
}

func (p *Player) GetQueue() []Track {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil
	}
	out := make([]Track, len(p.queue))
	copy(out, p.queue)
	return out
}

func (p *Player) NowPlaying() *Track {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentTrack == nil {
		return nil
	}
	cp := *p.currentTrack
	return &cp
}

func (p *Player) IsPlaying() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentTrack != nil
}

func (p *Player) Pause() {
	p.paused.Store(true)
}

func (p *Player) Resume() {
	if p.paused.Swap(false) {
		select {
		case p.resumeCh <- struct{}{}:
		default:
		}
	}
}

// ToggleLoop flips loop mode and returns the new state.
func (p *Player) ToggleLoop() bool {
	for {
		old := p.looping.Load()
		if p.looping.CompareAndSwap(old, !old) {
			return !old
		}
	}
}

func (p *Player) IsLooping() bool {
	return p.looping.Load()
}

func (p *Player) IsPaused() bool {
	return p.paused.Load()
}

// Skip cancels the current track; Run() will pick up the next one.
func (p *Player) Skip() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentCancel != nil {
		p.currentCancel()
	}
}

// Stop clears the queue, disables looping and cancels the current track.
func (p *Player) Stop() {
	p.ClearQueue()
	p.looping.Store(false)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentCancel != nil {
		p.currentCancel()
		p.currentCancel = nil
	}
}

func (p *Player) Run(ctx context.Context) {
	for {
		tr, ok := p.dequeue()
		if !ok {
			// Arm the idle timer only while connected; a nil channel blocks forever.
			var idleC <-chan time.Time
			if p.idleTimeout > 0 && p.Connected() {
				idleC = time.After(p.idleTimeout)
			}
			select {
			case <-ctx.Done():
				return
			case <-p.notifyCh:
				continue
			case <-idleC:
				// A track may have been enqueued in the same instant the
				// timer fired; don't disconnect under a freshly filled queue.
				if len(p.GetQueue()) > 0 {
					continue
				}
				log.Printf("guild %s: idle for %s, leaving voice", p.guildID, p.idleTimeout)
				p.notify(p.textChannel(), fmt.Sprintf("Left voice channel: idle for %s.", p.idleTimeout))
				if err := p.Disconnect(); err != nil {
					log.Printf("idle disconnect: %v", err)
				}
				continue
			}
		}
		p.postPanel(tr)
		// Replay the same track while loop mode is on; skip/stop cancel the
		// track context and break out, errors never replay.
		for {
			err := p.playTrack(ctx, tr)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("playback error [%s]: %v", tr.Title, err)
					p.notify(tr.ChannelID, fmt.Sprintf("⚠ Playback error for **%s**: %v", tr.Title, err))
				}
				break
			}
			if !p.looping.Load() {
				break
			}
		}
		if len(p.GetQueue()) == 0 {
			p.updatePanelIdle()
		}
	}
}

func (p *Player) textChannel() snowflake.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastTextCh
}

// postPanel replaces the interactive player panel: deletes the previous panel
// message (if any) and posts a fresh one for tr in the track's text channel.
func (p *Player) postPanel(tr Track) {
	channelID := tr.ChannelID
	if channelID == 0 {
		channelID = p.textChannel()
	}
	if channelID == 0 {
		return
	}

	p.mu.Lock()
	oldCh, oldMsg := p.panelChannelID, p.panelMessageID
	p.mu.Unlock()
	if oldMsg != 0 {
		_ = p.client.Rest.DeleteMessage(oldCh, oldMsg)
	}

	msg, err := p.client.Rest.CreateMessage(channelID, discord.MessageCreate{
		Embeds:     []discord.Embed{playerEmbed(&tr, p.IsPaused(), p.IsLooping())},
		Components: playerButtons(p.IsPaused(), p.IsLooping(), false),
	})
	if err != nil {
		log.Printf("post player panel: %v", err)
		return
	}
	p.mu.Lock()
	p.panelChannelID, p.panelMessageID = channelID, msg.ID
	p.mu.Unlock()
}

// updatePanelIdle greys out the panel once the queue has drained.
func (p *Player) updatePanelIdle() {
	p.mu.Lock()
	ch, msg := p.panelChannelID, p.panelMessageID
	p.mu.Unlock()
	if msg == 0 {
		return
	}
	_, err := p.client.Rest.UpdateMessage(ch, msg, discord.MessageUpdate{
		Embeds:     &[]discord.Embed{playerEmbed(nil, false, false)},
		Components: &[]discord.LayoutComponent{},
	})
	if err != nil {
		log.Printf("update player panel: %v", err)
	}
}

// notify posts msg to the given text channel; 0 or REST failures degrade to logs.
func (p *Player) notify(channelID snowflake.ID, msg string) {
	if channelID == 0 {
		return
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if _, err := p.client.Rest.CreateMessage(channelID, discord.MessageCreate{Content: msg}); err != nil {
		log.Printf("notify: %v", err)
	}
}

// announceSpeaking sends SetSpeaking with retries; the conn is fully open by
// the time this is called, so failures should be rare and transient.
func announceSpeaking(ctx context.Context, conn voice.Conn) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if err = conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return err
}

func (p *Player) playTrack(parent context.Context, tr Track) error {
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()

	if conn == nil {
		return errors.New("not connected to voice")
	}

	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	p.currentCancel = cancel
	cp := tr
	p.currentTrack = &cp
	if tr.ChannelID != 0 {
		p.lastTextCh = tr.ChannelID
	}
	p.mu.Unlock()
	defer func() {
		cancel()
		p.mu.Lock()
		p.currentCancel = nil
		p.currentTrack = nil
		p.mu.Unlock()
	}()

	// Join returns only after conn.Open() completes (SessionDescription received),
	// so SetSpeaking should succeed immediately. Retry as a safety net.
	if speakErr := announceSpeaking(ctx, conn); speakErr != nil {
		if errors.Is(speakErr, context.Canceled) {
			return speakErr
		}
		p.mu.Lock()
		if p.conn == conn {
			p.conn = nil
		}
		p.mu.Unlock()
		return fmt.Errorf("SetSpeaking: %w", speakErr)
	}
	defer func() {
		// Use a fresh context — the track context may already be cancelled.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = conn.SetSpeaking(stopCtx, voice.SpeakingFlagNone)
		time.Sleep(250 * time.Millisecond)
	}()

	// Googlevideo URLs go stale (IP-locked, expiring) and the network can cut
	// mid-track, so on a non-cancel failure re-resolve a fresh URL from the
	// original query and resume from where playback stopped.
	const maxRestarts = 2
	input := tr.Input
	var offset time.Duration
	for restart := 0; ; restart++ {
		played, err := p.streamOnce(ctx, conn, input, tr.IsURL, offset)
		offset += played
		if err == nil || errors.Is(err, context.Canceled) || restart >= maxRestarts {
			return err
		}
		log.Printf("stream interrupted at %s [%s], restart %d/%d: %v",
			offset.Round(time.Second), tr.Title, restart+1, maxRestarts, err)

		if tr.Query != "" && tr.IsURL {
			rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
			if res, rerr := youtube.Resolve(rctx, tr.Query); rerr == nil {
				input = res.StreamURL
			} else {
				log.Printf("re-resolve failed, reusing old URL: %v", rerr)
			}
			rcancel()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// streamOnce runs one ffmpeg pass over input (starting at offset) and streams
// it to Discord. Returns how much audio was actually sent, so the caller can
// resume from that position after a failure.
func (p *Player) streamOnce(parent context.Context, conn voice.Conn, input string, isURL bool, offset time.Duration) (time.Duration, error) {
	// Child context so an early sender exit unblocks the reader and kills
	// ffmpeg without cancelling the whole track (playTrack may restart us).
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	cmd, stdout, stderr, err := startFFmpeg(ctx, input, isURL, offset)
	if err != nil {
		return 0, err
	}
	defer stdout.Close()

	// Buffer 5 seconds of decoded frames so that brief source hiccups
	// do not immediately cause a Discord underrun.
	const bufferFrames = 250 // 250 × 20 ms = 5 s
	frameCh := make(chan []byte, bufferFrames)

	// Reader goroutine: FFmpeg stdout → frameCh.
	readErrCh := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(frameCh)
		readErrCh <- streamOggOpus(ctx, stdout, func(pkt []byte) error {
			select {
			case frameCh <- append([]byte(nil), pkt...):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	// Prebuffer ~2 s before the first frame goes out: a smooth start and a
	// cushion against jitter right after ffmpeg spins up.
	const prebufferFrames = 100
	prebufferDeadline := time.After(2500 * time.Millisecond)
PREBUFFER:
	for len(frameCh) < prebufferFrames {
		select {
		case <-ctx.Done():
			break PREBUFFER
		case <-readerDone: // short track fully decoded
			break PREBUFFER
		case <-prebufferDeadline:
			break PREBUFFER
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Sender: frameCh → Discord UDP with 20 ms pacing.
	var sendErr error
	framesSent := 0
	lastFrameSent := time.Now().UnixMilli()

SENDLOOP:
	for pkt := range frameCh {
		if p.paused.Load() {
			// Silence frames let the receiving decoder settle instead of
			// interpolating into a click.
			p.sendSilence(conn)
			for p.paused.Load() {
				select {
				case <-ctx.Done():
					sendErr = ctx.Err()
					break SENDLOOP
				case <-p.resumeCh:
					// Reset timing so we don't burst the buffered frames.
					lastFrameSent = time.Now().UnixMilli()
				}
			}
		}
		if err := ctx.Err(); err != nil {
			sendErr = err
			break
		}

		sleepTime := time.Duration(20-(time.Now().UnixMilli()-lastFrameSent)) * time.Millisecond
		if sleepTime > 0 {
			time.Sleep(sleepTime)
		}
		if time.Now().UnixMilli() < lastFrameSent+60 {
			lastFrameSent += 20
		} else {
			lastFrameSent = time.Now().UnixMilli()
		}

		if err := p.sendWithReconnect(ctx, pkt); err != nil {
			sendErr = err
			break
		}
		framesSent++
	}

	played := time.Duration(framesSent) * 20 * time.Millisecond

	// Unblock the reader goroutine if the sender exited early.
	if sendErr != nil {
		cancel()
	}
	streamErr := <-readErrCh

	if sendErr != nil || streamErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()

	if sendErr != nil {
		return played, sendErr
	}
	if streamErr != nil {
		if stderr.Len() > 0 && !errors.Is(streamErr, context.Canceled) {
			return played, fmt.Errorf("stream error: %w | ffmpeg: %s", streamErr, stderr.String())
		}
		return played, streamErr
	}
	if waitErr != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if stderr.Len() > 0 {
			return played, fmt.Errorf("ffmpeg wait: %w | ffmpeg: %s", waitErr, stderr.String())
		}
		return played, waitErr
	}

	// Clean end of stream: pad with silence so the decoder finishes cleanly.
	p.sendSilence(conn)
	return played, nil
}

// opusSilence is the Opus PLC silence frame; Discord expects ~5 of them after
// audio stops so the jitter buffer/decoder drains without artifacts.
var opusSilence = []byte{0xF8, 0xFF, 0xFE}

func (p *Player) sendSilence(conn voice.Conn) {
	for i := 0; i < 5; i++ {
		_ = safeUDPWrite(conn, opusSilence)
		time.Sleep(20 * time.Millisecond)
	}
}

// safeUDPWrite wraps conn.UDP().Write to recover from the nil-pointer panic
// that disgo raises when the internal UDP socket was closed by a 4006
// gateway error (udpConnImpl stores a non-nil struct but a nil net.PacketConn).
func safeUDPWrite(conn voice.Conn, pkt []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("udp write panicked (nil socket): %v", r)
		}
	}()
	_, err = conn.UDP().Write(pkt)
	return err
}

// safeConnClose calls conn.Close and recovers from the nil-pointer panic that
// disgo raises in udpConnImpl.Close when the UDP socket was never opened (i.e.
// the voice connection timed out before OpcodeReady triggered udp.Open).
// The leave VoiceStateUpdate is always sent by the first line of conn.Close
// (not deferred), so the bot exits the channel even when the panic fires.
func safeConnClose(conn voice.Conn, ctx context.Context) {
	defer func() { recover() }()
	conn.Close(ctx)
}

const voiceReconnectAttempts = 3

// sendWithReconnect sends pkt to Discord; on failure it performs a full
// session recreate (Join) up to voiceReconnectAttempts times. Using
// conn.Open() on the existing conn is not sufficient after a 4006 "Session is
// no longer valid" — a brand-new conn must be created each attempt.
// On final failure p.conn is left nil so the next /play triggers a fresh join.
func (p *Player) sendWithReconnect(ctx context.Context, pkt []byte) error {
	p.mu.Lock()
	conn := p.conn
	channelID := p.channelID
	p.mu.Unlock()

	if err := safeUDPWrite(conn, pkt); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	log.Println("voice write error, attempting full reconnect...")

	// Null out p.conn immediately. If all attempts fail it stays nil, so
	// p.Connected() returns false and the next /play triggers a clean join.
	p.mu.Lock()
	p.conn = nil
	p.mu.Unlock()

	for i := 1; i <= voiceReconnectAttempts; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := p.Join(channelID); err != nil {
			log.Printf("voice reconnect attempt %d/%d: %v", i, voiceReconnectAttempts, err)
		} else {
			p.mu.Lock()
			conn = p.conn
			p.mu.Unlock()

			// Re-announce speaking on the fresh connection.
			if err := announceSpeaking(ctx, conn); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}

			if err := safeUDPWrite(conn, pkt); err == nil {
				log.Printf("voice reconnected (attempt %d/%d)", i, voiceReconnectAttempts)
				return nil
			}

			// Write still failing; discard this session before the next attempt.
			p.mu.Lock()
			p.conn = nil
			p.mu.Unlock()
		}

		if i < voiceReconnectAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}

	// All attempts failed. Clean up VoiceManager so the next Join() creates a
	// fresh conn rather than reusing this dead one.
	p.client.VoiceManager.RemoveConn(p.guildID)
	return fmt.Errorf("voice reconnect failed after %d attempts", voiceReconnectAttempts)
}

func startFFmpeg(ctx context.Context, input string, isURL bool, offset time.Duration) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}

	// Reconnect flags are useful mainly for URL sources.
	if isURL {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_on_network_error", "1",
			"-reconnect_on_http_error", "4xx,5xx",
			"-reconnect_delay_max", "5",
			// Error out instead of hanging on a dead connection so the
			// restart/re-resolve loop in playTrack can take over.
			"-rw_timeout", "15000000", // 15 s, in µs
		)
	}

	if offset > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.2f", offset.Seconds()))
	}

	args = append(args,
		"-i", input,
		"-vn",
		"-map", "0:a:0",
		"-ac", "2",
		"-ar", "48000",
		"-c:a", "libopus",
		"-b:a", "128k",
		"-vbr", "on",
		"-frame_duration", "20",
		"-application", "audio",
		"-f", "ogg",
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("ffmpeg start: %w | %s", err, stderr.String())
	}

	return cmd, stdout, stderr, nil
}

func streamOggOpus(ctx context.Context, r io.Reader, onPacket func([]byte) error) error {
	header := make([]byte, 27)
	var continued bytes.Buffer

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, err := io.ReadFull(r, header)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("read ogg header: %w", err)
		}

		if !bytes.Equal(header[:4], []byte("OggS")) {
			return errors.New("invalid Ogg stream: missing OggS magic")
		}

		pageSegments := int(header[26])
		lacing := make([]byte, pageSegments)
		if _, err := io.ReadFull(r, lacing); err != nil {
			return fmt.Errorf("read lacing table: %w", err)
		}

		total := 0
		for _, seg := range lacing {
			total += int(seg)
		}

		payload := make([]byte, total)
		if _, err := io.ReadFull(r, payload); err != nil {
			return fmt.Errorf("read ogg payload: %w", err)
		}

		offset := 0
		for _, seg := range lacing {
			n := int(seg)
			continued.Write(payload[offset : offset+n])
			offset += n

			// seg < 255 marks the end of a packet.
			if seg < 255 {
				pkt := continued.Bytes()
				continued.Reset()

				// Skip Opus metadata packets.
				if bytes.HasPrefix(pkt, []byte("OpusHead")) || bytes.HasPrefix(pkt, []byte("OpusTags")) {
					continue
				}

				if err := onPacket(pkt); err != nil {
					return err
				}
			}
		}
	}
}
