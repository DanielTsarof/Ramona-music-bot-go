# Ramona-go

A Discord music bot for YouTube playback, written in Go.

## Features

- Play by URL, search query, or a whole YouTube playlist link (`/play`), queue of
  up to `MAX_QUEUE_SIZE` tracks
- Interactive player panel, auto-posted when a track starts — buttons:
  ⏸/▶ pause, ⏭ skip, ↺ loop, ☰ queue, ⏹ stop
- Loop the current track (`/loop` or the ↺ button)
- Auto-disconnect when the voice channel has no listeners left or nothing has
  played for `IDLE_DISCONNECT_SECONDS`
- Voice with DAVE E2EE support (Discord end-to-end encryption)
- Network resilience: yt-dlp self-update on startup, resolve retries, ffmpeg
  restart with resume from the interruption point, voice reconnection

## Commands

| Command | Description |
|---|---|
| `/play <query>` | Play a track (URL or search) or enqueue a whole playlist (playlist URL) |
| `/pause`, `/resume` | Pause / resume playback |
| `/skip` | Skip the current track |
| `/loop` | Toggle looping of the current track |
| `/stop` | Stop and clear the queue |
| `/join`, `/leave` | Join / leave the voice channel |
| `/queue`, `/nowplaying` | Show the queue / current track |
| `/ping`, `/help` | Health check / help |

## Getting started

1. Create `.env` in the project root:

```env
DISCORD_TOKEN=your_bot_token        # required
DISCORD_GUILD_ID=                   # guild ID for instant command sync (empty = global, up to 1 h)
MAX_QUEUE_SIZE=50                   # maximum queue size
IDLE_DISCONNECT_SECONDS=300         # idle disconnect timeout, 0 = disabled
YTDLP_COOKIES=/data/cookies.txt     # optional: YouTube cookies for yt-dlp (empty = off)
```

2. Build and run:

```sh
docker compose up --build
```

The yt-dlp binary is downloaded on first start and cached in the `ytdlp-cache`
volume (it self-updates to the latest version on every startup).

The image is multi-arch (`amd64`/`arm64`): it also builds and runs on a
Raspberry Pi 4 with 64-bit Raspberry Pi OS — run the same command on the Pi.
32-bit OS (armv7) is not supported (no libdave build for it).

### YouTube cookies (optional)

Cookies help against "Sign in to confirm you're not a bot", age-restricted
videos and throttling. Export them from a logged-in browser into
Netscape format:

```sh
yt-dlp --cookies-from-browser firefox --cookies data/cookies.txt -s "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
```

Put the file at `./data/cookies.txt` (the `data/` directory is mounted into the
container read-write — yt-dlp rewrites rotated cookies) and set
`YTDLP_COOKIES=/data/cookies.txt` in `.env`. Never commit this file — it is
your account session (`data/` is gitignored).

## Stack

- [disgo](https://github.com/disgoorg/disgo) — Discord API (gateway, voice, interactions)
- [golibdave](https://github.com/disgoorg/godave) + `libdave.so` — DAVE E2EE (cgo, installed in the Docker image)
- [go-ytdlp](https://github.com/lrstanley/go-ytdlp) — YouTube resolving via yt-dlp
- ffmpeg — decoding and encoding to Ogg/Opus (demuxing in Go, `modules/music/player.go`)

## Layout

```
internal/bot/       entry point, slash-command router
internal/config/    configuration from .env
modules/common/     /ping, /help
modules/music/      player, queue, panel, commands
modules/music/youtube/  track resolving via yt-dlp
```

A new module is a package under `modules/<name>` with `Commands()` and
`Handlers()` methods, registered in `internal/bot/main.go`.

> Building locally outside Docker requires libdave (`pkg-config dave`) due to
> cgo; Docker is the primary build path.
