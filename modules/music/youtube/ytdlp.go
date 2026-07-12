package youtube

import (
	"context"
	"fmt"
	"strings"
	"time"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

// cookiesFile, when set, is passed to every yt-dlp invocation (--cookies).
// Set once at startup before any Resolve call; not safe for concurrent writes.
var cookiesFile string

// SetCookiesFile enables passing a Netscape-format cookies.txt to yt-dlp.
func SetCookiesFile(path string) {
	cookiesFile = path
}

// newCommand returns the base yt-dlp command with the flags shared by all calls.
func newCommand() *ytdlp.Command {
	cmd := ytdlp.New().
		ForceIPv4().
		SocketTimeout(15).
		Retries("3").
		ExtractorRetries("3").
		NoUpdate()
	if cookiesFile != "" {
		cmd = cmd.Cookies(cookiesFile)
	}
	return cmd
}

// Info describes a resolved track.
type Info struct {
	StreamURL  string
	Title      string
	WebpageURL string
	Duration   int // seconds, 0 = unknown
}

// Resolve returns stream info for a YouTube URL or plain-text search query.
// Non-URL queries are prefixed with "ytsearch1:". Transient network failures
// (TLS resets under throttling) are retried once on top of yt-dlp's own --retries.
func Resolve(ctx context.Context, query string) (Info, error) {
	input := query
	if !strings.HasPrefix(query, "http://") && !strings.HasPrefix(query, "https://") {
		input = "ytsearch1:" + query
	}

	const attempts = 2
	for i := 1; ; i++ {
		info, err := resolveOnce(ctx, input)
		if err == nil || i >= attempts || ctx.Err() != nil {
			return info, err
		}
		select {
		case <-ctx.Done():
			return Info{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ResolvePlaylist fetches up to limit playlist entries with one fast
// --flat-playlist call (no per-video extraction). Returned Infos have an empty
// StreamURL — the player resolves each entry lazily right before playback.
func ResolvePlaylist(ctx context.Context, url string, limit int) (string, []Info, error) {
	if limit <= 0 {
		limit = 50
	}

	const attempts = 2
	var (
		title   string
		entries []Info
		err     error
	)
	for i := 1; ; i++ {
		title, entries, err = resolvePlaylistOnce(ctx, url, limit)
		if err == nil || i >= attempts || ctx.Err() != nil {
			return title, entries, err
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func resolvePlaylistOnce(ctx context.Context, url string, limit int) (string, []Info, error) {
	result, err := newCommand().
		FlatPlaylist().
		PlaylistItems(fmt.Sprintf("1:%d", limit)).
		DumpJSON().
		Run(ctx, url)
	if err != nil {
		return "", nil, fmt.Errorf("yt-dlp: %w", err)
	}

	infos, err := result.GetExtractedInfo()
	if err != nil {
		return "", nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	if len(infos) == 0 {
		return "", nil, fmt.Errorf("playlist is empty or unavailable")
	}

	var title string
	if infos[0].PlaylistTitle != nil {
		title = *infos[0].PlaylistTitle
	}

	entries := make([]Info, 0, len(infos))
	for _, raw := range infos {
		e := Info{}
		if raw.Title != nil {
			e.Title = *raw.Title
		}
		if e.Title == "" {
			e.Title = raw.ID
		}
		if raw.Duration != nil {
			e.Duration = int(*raw.Duration)
		}
		// Flat entries carry the video URL in URL; fall back to building one from the ID.
		switch {
		case raw.URL != nil && *raw.URL != "":
			e.WebpageURL = *raw.URL
		case raw.ID != "":
			e.WebpageURL = "https://www.youtube.com/watch?v=" + raw.ID
		default:
			continue // no way to play this entry
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("playlist has no playable entries")
	}
	return title, entries, nil
}

func resolveOnce(ctx context.Context, input string) (Info, error) {
	result, err := newCommand().
		NoPlaylist().
		Format("bestaudio[ext=webm]/bestaudio/best").
		DumpJSON().
		Run(ctx, input)
	if err != nil {
		return Info{}, fmt.Errorf("yt-dlp: %w", err)
	}

	infos, err := result.GetExtractedInfo()
	if err != nil {
		return Info{}, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	if len(infos) == 0 {
		return Info{}, fmt.Errorf("yt-dlp returned no results")
	}

	raw := infos[0]
	out := Info{}
	if raw.Title != nil {
		out.Title = *raw.Title
	}
	if raw.WebpageURL != nil {
		out.WebpageURL = *raw.WebpageURL
	}
	if raw.Duration != nil {
		out.Duration = int(*raw.Duration)
	}

	// raw.URL is the top-level selected-format URL; fall back to the embedded format.
	switch {
	case raw.URL != nil && *raw.URL != "":
		out.StreamURL = *raw.URL
	case raw.ExtractedFormat != nil && raw.ExtractedFormat.URL != "":
		out.StreamURL = raw.ExtractedFormat.URL
	default:
		return Info{}, fmt.Errorf("yt-dlp returned no stream URL")
	}
	return out, nil
}
