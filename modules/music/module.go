package music

import (
	"context"
	"sync"

	"RamonaGo/internal/config"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

type MusicModule struct {
	client  *bot.Client
	cfg     *config.Config
	players map[snowflake.ID]*Player
	mu      sync.Mutex
	ctx     context.Context
}

func NewMusicModule(client *bot.Client, cfg *config.Config, ctx context.Context) *MusicModule {
	return &MusicModule{
		client:  client,
		cfg:     cfg,
		players: make(map[snowflake.ID]*Player),
		ctx:     ctx,
	}
}

func (m *MusicModule) getOrCreatePlayer(guildID snowflake.ID) *Player {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.players[guildID]; ok {
		return p
	}
	p := NewPlayer(m.client, guildID, m.cfg.MaxQueueSize)
	go p.Run(m.ctx)
	m.players[guildID] = p
	return p
}

func (m *MusicModule) Commands() []discord.ApplicationCommandCreate {
	return musicCommands
}

func (m *MusicModule) Handlers() map[string]func(*events.ApplicationCommandInteractionCreate) {
	return map[string]func(*events.ApplicationCommandInteractionCreate){
		"play":       m.handlePlay,
		"pause":      m.handlePause,
		"resume":     m.handleResume,
		"skip":       m.handleSkip,
		"stop":       m.handleStop,
		"join":       m.handleJoin,
		"leave":      m.handleLeave,
		"queue":      m.handleQueue,
		"nowplaying": m.handleNowPlaying,
	}
}
