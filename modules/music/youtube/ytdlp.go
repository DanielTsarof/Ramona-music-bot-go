package youtube

import (
	"context"
	"fmt"
	"strings"
	"time"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

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

func resolveOnce(ctx context.Context, input string) (Info, error) {
	result, err := ytdlp.New().
		NoPlaylist().
		Format("bestaudio[ext=webm]/bestaudio/best").
		ForceIPv4().
		SocketTimeout(15).
		Retries("3").
		ExtractorRetries("3").
		NoUpdate().
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
