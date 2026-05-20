package config

import (
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

type Config struct {
	DiscordToken string `env:"DISCORD_TOKEN" env-required:"true"`
	GuildID      string `env:"DISCORD_GUILD_ID"`
	MaxQueueSize int    `env:"MAX_QUEUE_SIZE" env-default:"50"`
}

func Load() (*Config, error) {
	var cfg Config
	if _, err := os.Stat(".env"); err == nil {
		return &cfg, cleanenv.ReadConfig(".env", &cfg)
	}
	return &cfg, cleanenv.ReadEnv(&cfg)
}
