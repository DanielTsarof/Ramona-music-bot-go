package common

import (
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

type CommonModule struct{}

func New() *CommonModule { return &CommonModule{} }

var commonCommands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{Name: "ping", Description: "Check bot responsiveness"},
	discord.SlashCommandCreate{Name: "help", Description: "Show all available commands"},
}

func (m *CommonModule) Commands() []discord.ApplicationCommandCreate {
	return commonCommands
}

func (m *CommonModule) Handlers() map[string]func(*events.ApplicationCommandInteractionCreate) {
	return map[string]func(*events.ApplicationCommandInteractionCreate){
		"ping": m.handlePing,
		"help": m.handleHelp,
	}
}

func (m *CommonModule) handlePing(e *events.ApplicationCommandInteractionCreate) {
	_ = e.CreateMessage(discord.MessageCreate{Content: "Pong!"})
}

func (m *CommonModule) handleHelp(e *events.ApplicationCommandInteractionCreate) {
	const msg = "**Music**\n" +
		"`/play <query>` — Play a song (URL or search)\n" +
		"`/pause` — Pause playback\n" +
		"`/resume` — Resume playback\n" +
		"`/skip` — Skip the current track\n" +
		"`/stop` — Stop and clear the queue\n" +
		"`/join` — Join your voice channel\n" +
		"`/leave` — Leave the voice channel\n" +
		"`/queue` — Show the queue\n" +
		"`/nowplaying` — Show the current track\n\n" +
		"**General**\n" +
		"`/ping` — Check responsiveness\n" +
		"`/help` — Show this message"

	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}
