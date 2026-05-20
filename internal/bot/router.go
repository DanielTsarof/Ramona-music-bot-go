package main

import (
	"context"
	"fmt"
	"log"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

type Router struct {
	client   *bot.Client
	guildID  snowflake.ID // 0 = global commands
	cmds     []discord.ApplicationCommandCreate
	handlers map[string]func(*events.ApplicationCommandInteractionCreate)
}

func NewRouter(client *bot.Client, guildID snowflake.ID) *Router {
	return &Router{
		client:   client,
		guildID:  guildID,
		handlers: make(map[string]func(*events.ApplicationCommandInteractionCreate)),
	}
}

func (r *Router) Register(m Module) {
	r.cmds = append(r.cmds, m.Commands()...)
	for name, h := range m.Handlers() {
		r.handlers[name] = h
	}
}

// Sync atomically replaces all slash commands (guild or global) with the
// registered set. Safe to call on every startup.
func (r *Router) Sync(ctx context.Context) error {
	appID := r.client.ID()
	var err error
	if r.guildID != 0 {
		_, err = r.client.Rest.SetGuildCommands(appID, r.guildID, r.cmds)
	} else {
		_, err = r.client.Rest.SetGlobalCommands(appID, r.cmds)
	}
	if err != nil {
		return fmt.Errorf("set commands: %w", err)
	}
	log.Printf("synced %d slash commands (guildID=%v)", len(r.cmds), r.guildID)
	return nil
}

func (r *Router) OnEvent(e *events.ApplicationCommandInteractionCreate) {
	name := e.SlashCommandInteractionData().CommandName()
	if h, ok := r.handlers[name]; ok {
		h(e)
	} else {
		log.Printf("unhandled command: %s", name)
	}
}
