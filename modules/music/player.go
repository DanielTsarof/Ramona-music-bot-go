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
	p.mu.Lock()
	conn := p.client.VoiceManager.GetConn(p.guildID)
	if conn == nil {
		conn = p.client.VoiceManager.CreateConn(p.guildID)
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.Open(ctx, channelID, false, false); err != nil {
		return fmt.Errorf("voice join: %w", err)
	}

	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()

	// Small buffer before playback.
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

	cmd, stdout, stderr, err := startFFmpeg(ctx, tr)
	if err != nil {
		return err
	}
	defer stdout.Close()

	if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("SetSpeaking: %w", err)
	}
	defer func() {
		// Use a fresh context — the track context may already be cancelled.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = conn.SetSpeaking(stopCtx, voice.SpeakingFlagNone)
		time.Sleep(250 * time.Millisecond)
	}()

	lastFrameSent := time.Now().UnixMilli()

	streamErr := streamOggOpus(ctx, stdout, func(pkt []byte) error {
		for p.paused.Load() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.resumeCh:
			}
		}
		if err := ctx.Err(); err != nil {
			return err
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

		_, err := conn.UDP().Write(pkt)
		return err
	})

	if streamErr != nil {
		_ = cmd.Process.Kill()
	}

	waitErr := cmd.Wait()

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
				pkt := append([]byte(nil), continued.Bytes()...)
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
