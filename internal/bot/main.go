package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"RamonaGo/internal/config"
	"RamonaGo/modules/common"
	"RamonaGo/modules/music"
	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
	"github.com/disgoorg/snowflake/v2"
	ytdlp "github.com/lrstanley/go-ytdlp"
)

func main() {
	// AllowVersionMismatch: go-ytdlp pins an old yt-dlp release; we self-update
	// the cached binary below, and the next startup must not clobber it back.
	if _, err := ytdlp.Install(context.Background(), &ytdlp.InstallOptions{AllowVersionMismatch: true}); err != nil {
		log.Fatalf("yt-dlp install: %v", err)
	}
	updateCtx, updateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if _, err := ytdlp.New().UpdateTo(updateCtx, "stable@latest"); err != nil {
		log.Printf("WARN yt-dlp self-update failed (continuing with cached binary): %v", err)
	}
	updateCancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var guildID snowflake.ID
	if cfg.GuildID != "" {
		guildID, err = snowflake.Parse(cfg.GuildID)
		if err != nil {
			log.Fatalf("invalid guild ID: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := disgo.New(cfg.DiscordToken,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds|
					gateway.IntentGuildMessages|
					gateway.IntentMessageContent|
					gateway.IntentGuildVoiceStates,
			),
		),
		bot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagVoiceStates),
		),
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
		),
		bot.WithEventManagerConfigOpts(
			bot.WithAsyncEventsEnabled(),
		),
	)
	if err != nil {
		log.Fatalf("disgo.New: %v", err)
	}

	router := NewRouter(client, guildID)
	router.Register(common.New())
	router.Register(music.NewMusicModule(client, cfg, ctx))

	client.AddEventListeners(
		bot.NewListenerFunc(func(e *events.Ready) {
			log.Printf("logged in as %s", e.User.Username)
			if err := router.Sync(context.Background()); err != nil {
				log.Printf("sync commands: %v", err)
			}
		}),
		bot.NewListenerFunc(router.OnEvent),
	)

	if err := client.OpenGateway(ctx); err != nil {
		log.Fatalf("OpenGateway: %v", err)
	}
	defer client.Close(context.Background())

	log.Println("bot is running. Press Ctrl+C to stop.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
}
