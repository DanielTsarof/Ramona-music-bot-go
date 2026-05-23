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

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

type Track struct {
	Input string // local path or direct stream URL
	IsURL bool
	Title string
}

type Player struct {
	client  *bot.Client
	guildID snowflake.ID

	mu            sync.Mutex
	conn          voice.Conn
	channelID     snowflake.ID // stored for reconnect
	currentCancel context.CancelFunc
	currentTrack  *Track

	queue    []Track
	notifyCh chan struct{}
	maxSize  int

	resumeCh chan struct{}
	paused   atomic.Bool
}

func NewPlayer(client *bot.Client, guildID snowflake.ID, maxSize int) *Player {
	if maxSize <= 0 {
		maxSize = 50
	}
	return &Player{
		client:   client,
		guildID:  guildID,
		notifyCh: make(chan struct{}, 1),
		maxSize:  maxSize,
		resumeCh: make(chan struct{}, 1),
	}
}

func (p *Player) Join(channelID snowflake.ID) error {
	// Discard any stale conn (e.g. from a previous failed Open).
	if p.client.VoiceManager.GetConn(p.guildID) != nil {
		p.client.VoiceManager.RemoveConn(p.guildID)
	}
	conn := p.client.VoiceManager.CreateConn(p.guildID)

	// conn.Open blocks until the full DAVE MLS handshake completes, which can
	// take 20-30 s. The UDP layer is established well before that and is all we
	// need to start sending audio. Run Open in a background goroutine and return
	// as soon as conn.UDP() is non-nil; DAVE finishes on its own after that.
	openCtx, openCancel := context.WithTimeout(context.Background(), 60*time.Second)
	openErrCh := make(chan error, 1)
	go func() {
		openErrCh <- conn.Open(openCtx, channelID, false, false)
		openCancel()
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// If UDP isn't up within 15 s something is genuinely wrong.
	hardDeadline := time.NewTimer(15 * time.Second)
	defer hardDeadline.Stop()

	var openErr error
	ready := false
	for !ready {
		select {
		case err := <-openErrCh:
			// Open finished (success = DAVE done; error = failed before UDP).
			openErr = err
			ready = true
		case <-ticker.C:
			// UDP layer is up; DAVE may still be negotiating in the background.
			ready = conn.UDP() != nil
		case <-hardDeadline.C:
			openCancel()
			p.client.VoiceManager.RemoveConn(p.guildID)
			return fmt.Errorf("voice join: timed out waiting for UDP connection")
		}
	}

	if openErr != nil {
		p.client.VoiceManager.RemoveConn(p.guildID)
		return fmt.Errorf("voice join: %w", openErr)
	}

	p.mu.Lock()
	p.conn = conn
	p.channelID = channelID
	p.mu.Unlock()

	time.Sleep(250 * time.Millisecond)
	return nil
}

func (p *Player) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil
}

func (p *Player) Disconnect() error {
	p.Stop()
	p.mu.Lock()
	conn := p.conn
	p.conn = nil
	p.channelID = 0
	p.mu.Unlock()

	if conn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Close(ctx)
	p.client.VoiceManager.RemoveConn(p.guildID)
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

// Skip cancels the current track; Run() will pick up the next one.
func (p *Player) Skip() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentCancel != nil {
		p.currentCancel()
	}
}

// Stop clears the queue and cancels the current track.
func (p *Player) Stop() {
	p.ClearQueue()
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
			select {
			case <-ctx.Done():
				return
			case <-p.notifyCh:
				continue
			}
		}
		if err := p.playTrack(ctx, tr); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("playback error: %v", err)
		}
	}
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
	p.mu.Unlock()
	defer func() {
		cancel()
		p.mu.Lock()
		p.currentCancel = nil
		p.currentTrack = nil
		p.mu.Unlock()
	}()

	// After Join returns (UDP up), the voice session_description exchange may
	// still be in progress. SetSpeaking fails with "shard is not ready" until it
	// completes. Retry for up to 10 s before starting FFmpeg.
	var speakErr error
	for attempt := 0; attempt < 60; attempt++ {
		speakErr = conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone)
		if speakErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if speakErr != nil {
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

	cmd, stdout, stderr, err := startFFmpeg(ctx, tr)
	if err != nil {
		return err
	}
	defer stdout.Close()

	// Buffer 2 seconds of decoded frames so that brief source hiccups
	// do not immediately cause a Discord underrun.
	const bufferFrames = 100 // 100 × 20 ms = 2 s
	frameCh := make(chan []byte, bufferFrames)

	// Reader goroutine: FFmpeg stdout → frameCh.
	readErrCh := make(chan error, 1)
	go func() {
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

	// Sender: frameCh → Discord UDP with 20 ms pacing.
	var sendErr error
	lastFrameSent := time.Now().UnixMilli()

SENDLOOP:
	for pkt := range frameCh {
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
		if sendErr != nil {
			break
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
	}

	// Unblock the reader goroutine if the sender exited early.
	if sendErr != nil {
		cancel()
	}
	streamErr := <-readErrCh

	if sendErr != nil {
		_ = cmd.Process.Kill()
	}
	if streamErr != nil {
		_ = cmd.Process.Kill()
	}

	waitErr := cmd.Wait()

	if sendErr != nil {
		return sendErr
	}
	if streamErr != nil {
		if stderr.Len() > 0 && !errors.Is(streamErr, context.Canceled) {
			return fmt.Errorf("stream error: %w | ffmpeg: %s", streamErr, stderr.String())
		}
		return streamErr
	}
	if waitErr != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if stderr.Len() > 0 {
			return fmt.Errorf("ffmpeg wait: %w | ffmpeg: %s", waitErr, stderr.String())
		}
		return waitErr
	}

	return nil
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
			for j := 0; j < 10; j++ {
				if conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone) == nil {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
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

	return fmt.Errorf("voice reconnect failed after %d attempts", voiceReconnectAttempts)
}

func startFFmpeg(ctx context.Context, tr Track) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}

	// Reconnect flags are useful mainly for URL sources.
	if tr.IsURL {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
		)
	}

	args = append(args,
		"-i", tr.Input,
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
