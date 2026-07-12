package config

import (
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

type Config struct {
	DiscordToken string `env:"DISCORD_TOKEN" env-required:"true"`
	GuildID      string `env:"DISCORD_GUILD_ID"`
	MaxQueueSize int    `env:"MAX_QUEUE_SIZE" env-default:"50"`
	// IdleDisconnectSeconds: leave voice after this long with nothing playing (0 = never).
	IdleDisconnectSeconds int `env:"IDLE_DISCONNECT_SECONDS" env-default:"300"`
	// YtdlpCookies: path to a Netscape-format cookies.txt passed to yt-dlp (empty = off).
	YtdlpCookies string `env:"YTDLP_COOKIES"`
}

func Load() (*Config, error) {
	var cfg Config
	if _, err := os.Stat(".env"); err == nil {
		return &cfg, cleanenv.ReadConfig(".env", &cfg)
	}
	return &cfg, cleanenv.ReadEnv(&cfg)
}
