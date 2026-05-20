package youtube

import (
	"context"
	"fmt"
	"strings"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

// Resolve returns a direct audio stream URL and title for a YouTube URL or
// plain-text search query. Non-URL queries are prefixed with "ytsearch1:".
func Resolve(ctx context.Context, query string) (streamURL, title string, err error) {
	input := query
	if !strings.HasPrefix(query, "http://") && !strings.HasPrefix(query, "https://") {
		input = "ytsearch1:" + query
	}

	result, err := ytdlp.New().
		NoPlaylist().
		Format("bestaudio[ext=webm]/bestaudio/best").
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
