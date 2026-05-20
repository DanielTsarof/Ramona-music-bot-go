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

	"github.com/bwmarrin/discordgo"
)

type Track struct {
	Input string // local path or URL
	IsURL bool
	Title string
}

type Player struct {
	s       *discordgo.Session
	guildID string

	mu            sync.Mutex
	vc            *discordgo.VoiceConnection
	currentCancel context.CancelFunc

	queue    chan Track
	resumeCh chan struct{}
	paused   atomic.Bool
}

func NewPlayer(s *discordgo.Session, guildID string, queueSize int) *Player {
	if queueSize <= 0 {
		queueSize = 32
	}
	return &Player{
		s:        s,
		guildID:  guildID,
		queue:    make(chan Track, queueSize),
		resumeCh: make(chan struct{}, 1),
	}
}

func (p *Player) Join(channelID string) error {
	vc, err := p.s.ChannelVoiceJoin(p.guildID, channelID, false, true)
	if err != nil {
		return fmt.Errorf("voice join: %w", err)
	}

	p.mu.Lock()
	old := p.vc
	p.vc = vc
	p.mu.Unlock()

	if old != nil && old != vc && old.ChannelID != channelID {
		_ = old.Disconnect()
	}

	// По аналогии с официальным airhorn example — небольшой буфер до playback.
	time.Sleep(250 * time.Millisecond)
	return nil
}

func (p *Player) Disconnect() error {
	p.Stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vc == nil {
		return nil
	}
	err := p.vc.Disconnect()
	p.vc = nil
	return err
}

func (p *Player) Enqueue(t Track) error {
	select {
	case p.queue <- t:
		return nil
	default:
		return errors.New("queue is full")
	}
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

func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentCancel != nil {
		p.currentCancel()
		p.currentCancel = nil
	}
}

func (p *Player) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case tr := <-p.queue:
			if err := p.playTrack(ctx, tr); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("playback error: %v", err)
			}
		}
	}
}

func (p *Player) playTrack(parent context.Context, tr Track) error {
	p.mu.Lock()
	vc := p.vc
	p.mu.Unlock()

	if vc == nil {
		return errors.New("not connected to voice")
	}

	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	p.currentCancel = cancel
	p.mu.Unlock()
	defer func() {
		cancel()
		p.mu.Lock()
		p.currentCancel = nil
		p.mu.Unlock()
	}()

	cmd, stdout, stderr, err := startFFmpeg(ctx, tr)
	if err != nil {
		return err
	}
	defer stdout.Close()

	if err := vc.Speaking(true); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("speaking(true): %w", err)
	}
	defer func() {
		_ = vc.Speaking(false)
		time.Sleep(250 * time.Millisecond)
	}()

	streamErr := streamOggOpus(ctx, stdout, func(pkt []byte) error {
		for p.paused.Load() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.resumeCh:
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case vc.OpusSend <- pkt:
			return nil
		}
	})

	if streamErr != nil {
		_ = cmd.Process.Kill()
	}

	waitErr := cmd.Wait()

	if streamErr != nil {
		if stderr.Len() > 0 {
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

			// seg < 255 means end of packet
			if seg < 255 {
				pkt := append([]byte(nil), continued.Bytes()...)
				continued.Reset()

				// Skip Opus metadata packets
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
