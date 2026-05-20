package main

import (
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

// Module is implemented by any command group registered with the Router.
// Using concrete types from the disgo packages lets external packages satisfy
// this interface without importing package main.
type Module interface {
	Commands() []discord.ApplicationCommandCreate
	Handlers() map[string]func(*events.ApplicationCommandInteractionCreate)
}
