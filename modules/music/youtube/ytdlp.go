package youtube

import (
	"context"
	"fmt"
	"strings"
	"time"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

// Resolve returns a direct audio stream URL and title for a YouTube URL or
// plain-text search query. Non-URL queries are prefixed with "ytsearch1:".
// Transient network failures (TLS resets under throttling) are retried once
// on top of yt-dlp's own --retries.
func Resolve(ctx context.Context, query string) (streamURL, title string, err error) {
	input := query
	if !strings.HasPrefix(query, "http://") && !strings.HasPrefix(query, "https://") {
		input = "ytsearch1:" + query
	}

	const attempts = 2
	for i := 1; ; i++ {
		streamURL, title, err = resolveOnce(ctx, input)
		if err == nil || i >= attempts || ctx.Err() != nil {
			return streamURL, title, err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func resolveOnce(ctx context.Context, input string) (streamURL, title string, err error) {
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
		return "", "", fmt.Errorf("yt-dlp: %w", err)
	}

	infos, err := result.GetExtractedInfo()
	if err != nil {
		return "", "", fmt.Errorf("parse yt-dlp output: %w", err)
	}
	if len(infos) == 0 {
		return "", "", fmt.Errorf("yt-dlp returned no results")
	}

	info := infos[0]

	if info.Title != nil {
		title = *info.Title
	}

	// info.URL is the top-level selected-format URL; fall back to the embedded format.
	if info.URL != nil && *info.URL != "" {
		return *info.URL, title, nil
	}
	if info.ExtractedFormat != nil && info.ExtractedFormat.URL != "" {
		return info.ExtractedFormat.URL, title, nil
	}

	return "", "", fmt.Errorf("yt-dlp returned no stream URL")
}
