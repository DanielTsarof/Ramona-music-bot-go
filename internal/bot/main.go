package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

func main() {
	token := "***REMOVED_DISCORD_TOKEN***" //os.Getenv("DISCORD_TOKEN")
	guildID := os.Getenv("DISCORD_GUILD_ID")                                          // optional; use for fast dev iteration

	if token == "" {
		log.Fatal("DISCORD_TOKEN is empty")
	}

	s, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("discordgo.New: %v", err)
	}

	// Нужны guild events, message events, message content для prefix-команд
	// и voice states на будущее.
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsGuildVoiceStates

	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("logged in as %s#%s", s.State.User.Username, s.State.User.Discriminator)
	})

	// Prefix-command: !ping
	s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}
		if m.Content == "!ping" {
			if _, err := s.ChannelMessageSend(m.ChannelID, "pong"); err != nil {
				log.Printf("ChannelMessageSend: %v", err)
			}
			log.Println("Ping message sent successfully")
		}
	})

	// Slash-command handler
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}

		switch i.ApplicationCommandData().Name {
		case "ping":
			err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "pong",
				},
			})
			if err != nil {
				log.Printf("InteractionRespond: %v", err)
			}
		}
	})

	if err := s.Open(); err != nil {
		log.Fatalf("Open: %v", err)
	}
	defer s.Close()

	cmd := &discordgo.ApplicationCommand{
		Name:        "ping",
		Description: "Проверка доступности бота",
	}

	registered, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, cmd)
	if err != nil {
		log.Fatalf("ApplicationCommandCreate: %v", err)
	}
	defer func() {
		// Для dev удобно удалять guild-команду на выходе.
		// Для global-команд это обычно не делают автоматически.
		if guildID != "" && registered != nil {
			_ = s.ApplicationCommandDelete(s.State.User.ID, guildID, registered.ID)
		}
	}()

	log.Println("bot is running")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}
