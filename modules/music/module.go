package music

import (
	"context"
	"log"
	"sync"
	"time"

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
	m := &MusicModule{
		client:  client,
		cfg:     cfg,
		players: make(map[snowflake.ID]*Player),
		ctx:     ctx,
	}
	client.AddEventListeners(bot.NewListenerFunc(m.onVoiceStateUpdate))
	return m
}

func (m *MusicModule) getOrCreatePlayer(guildID snowflake.ID) *Player {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.players[guildID]; ok {
		return p
	}
	idle := time.Duration(m.cfg.IdleDisconnectSeconds) * time.Second
	p := NewPlayer(m.client, guildID, m.cfg.MaxQueueSize, idle)
	go p.Run(m.ctx)
	m.players[guildID] = p
	return p
}

// getPlayer returns the guild's player without creating one.
func (m *MusicModule) getPlayer(guildID snowflake.ID) *Player {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.players[guildID]
}

// onVoiceStateUpdate leaves the voice channel when no listeners remain, and
// resets player state if the bot itself was removed from the channel.
func (m *MusicModule) onVoiceStateUpdate(e *events.GuildVoiceStateUpdate) {
	p := m.getPlayer(e.VoiceState.GuildID)
	if p == nil {
		return
	}

	botID := m.client.ID()

	// The bot itself left / was kicked or moved out: drop stale state so the
	// next /play does a clean join. (No-op echo after our own Disconnect.)
	if e.VoiceState.UserID == botID {
		if e.VoiceState.ChannelID == nil && p.Connected() {
			log.Printf("guild %s: bot removed from voice channel, resetting player", e.VoiceState.GuildID)
			p.ResetVoiceState()
		}
		return
	}

	botChannel := p.VoiceChannelID()
	if botChannel == 0 {
		return
	}

	// Count remaining occupants of the bot's channel. Other bots count as
	// listeners — telling them apart needs the members cache/intent.
	listeners := 0
	for vs := range m.client.Caches.VoiceStates(e.VoiceState.GuildID) {
		if vs.UserID != botID && vs.ChannelID != nil && *vs.ChannelID == botChannel {
			listeners++
		}
	}
	if listeners > 0 {
		return
	}

	log.Printf("guild %s: voice channel empty, leaving", e.VoiceState.GuildID)
	p.notify(p.textChannel(), "Left voice channel: no listeners.")
	if err := p.Disconnect(); err != nil {
		log.Printf("empty-channel disconnect: %v", err)
	}
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
		"loop":       m.handleLoop,
		"stop":       m.handleStop,
		"join":       m.handleJoin,
		"leave":      m.handleLeave,
		"queue":      m.handleQueue,
		"nowplaying": m.handleNowPlaying,
	}
}
