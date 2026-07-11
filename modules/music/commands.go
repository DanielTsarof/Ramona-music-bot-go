package music

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"RamonaGo/modules/music/youtube"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

var musicCommands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "play",
		Description: "Play a song — YouTube URL or search query",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "query",
				Description: "YouTube URL or search query",
				Required:    true,
			},
		},
	},
	discord.SlashCommandCreate{Name: "pause", Description: "Pause playback"},
	discord.SlashCommandCreate{Name: "resume", Description: "Resume paused playback"},
	discord.SlashCommandCreate{Name: "skip", Description: "Skip the current track"},
	discord.SlashCommandCreate{Name: "stop", Description: "Stop playback and clear the queue"},
	discord.SlashCommandCreate{Name: "join", Description: "Join your voice channel"},
	discord.SlashCommandCreate{Name: "leave", Description: "Leave the voice channel"},
	discord.SlashCommandCreate{Name: "queue", Description: "Show the current queue"},
	discord.SlashCommandCreate{Name: "nowplaying", Description: "Show the currently playing track"},
}

// findUserVoiceChannel returns the voice channel ID the invoking user is in.
func findUserVoiceChannel(e *events.ApplicationCommandInteractionCreate) (snowflake.ID, error) {
	guildID := e.GuildID()
	if guildID == nil {
		return 0, errors.New("not in a guild")
	}
	member := e.Member()
	if member == nil {
		return 0, errors.New("member not found")
	}
	vs, ok := e.Client().Caches.VoiceState(*guildID, member.User.ID)
	if !ok || vs.ChannelID == nil {
		return 0, errors.New("join a voice channel first")
	}
	return *vs.ChannelID, nil
}

func respond(e *events.ApplicationCommandInteractionCreate, msg string) {
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

func respondEphemeral(e *events.ApplicationCommandInteractionCreate, msg string) {
	_ = e.CreateMessage(discord.MessageCreate{
		Content: msg,
		Flags:   discord.MessageFlagEphemeral,
	})
}

func editResponse(e *events.ApplicationCommandInteractionCreate, msg string) {
	_, _ = e.Client().Rest.UpdateInteractionResponse(
		e.ApplicationID(), e.Token(),
		discord.MessageUpdate{}.WithContent(msg),
	)
}

func guildIDOf(e *events.ApplicationCommandInteractionCreate) (snowflake.ID, bool) {
	id := e.GuildID()
	if id == nil {
		respondEphemeral(e, "This command can only be used in a server.")
		return 0, false
	}
	return *id, true
}

// handlePlay defers the response immediately (yt-dlp can take a few seconds)
// then edits it once the track is resolved and queued.
func (m *MusicModule) handlePlay(e *events.ApplicationCommandInteractionCreate) {
	query := e.SlashCommandInteractionData().String("query")

	if err := e.DeferCreateMessage(false); err != nil {
		return
	}

	channelID, err := findUserVoiceChannel(e)
	if err != nil {
		editResponse(e, "You must be in a voice channel.")
		return
	}

	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)

	// Join before resolving: DAVE's MLS handshake is CPU-intensive; running
	// yt-dlp concurrently steals cycles and can push the handshake past its
	// timeout. Resolve after the connection is established.
	// Connected() (not a VoiceManager check): if a reconnect is in flight,
	// Join blocks on joinMu and then reuses the fresh conn, whereas skipping
	// Join with p.conn still nil would drop the track in playTrack.
	if !p.Connected() {
		editResponse(e, "Connecting to voice… (first join can take ~30s)")
		if err := p.Join(channelID); err != nil {
			editResponse(e, fmt.Sprintf("Could not join voice channel: %v", err))
			return
		}
	}

	editResponse(e, fmt.Sprintf("Searching: **%s**…", query))
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer resolveCancel()
	streamURL, title, err := youtube.Resolve(resolveCtx, query)
	if err != nil {
		editResponse(e, fmt.Sprintf("Could not resolve track: %v", err))
		return
	}

	if err := p.Enqueue(Track{Input: streamURL, IsURL: true, Title: title, Query: query, ChannelID: e.Channel().ID()}); err != nil {
		editResponse(e, fmt.Sprintf("Queue error: %v", err))
		return
	}

	editResponse(e, fmt.Sprintf("Queued: **%s**", title))
}

func (m *MusicModule) handlePause(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	if !p.IsPlaying() {
		respondEphemeral(e, "Nothing is playing.")
		return
	}
	p.Pause()
	respond(e, "Paused.")
}

func (m *MusicModule) handleResume(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	p.Resume()
	respond(e, "Resumed.")
}

func (m *MusicModule) handleSkip(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	if !p.IsPlaying() {
		respondEphemeral(e, "Nothing is playing.")
		return
	}
	p.Skip()
	respond(e, "Skipped.")
}

func (m *MusicModule) handleStop(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	p.Stop()
	respond(e, "Stopped and queue cleared.")
}

func (m *MusicModule) handleJoin(e *events.ApplicationCommandInteractionCreate) {
	channelID, err := findUserVoiceChannel(e)
	if err != nil {
		respondEphemeral(e, "You must be in a voice channel.")
		return
	}
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}

	if err := e.DeferCreateMessage(false); err != nil {
		return
	}

	p := m.getOrCreatePlayer(guildID)
	if !p.Connected() {
		editResponse(e, "Connecting to voice… (first join can take ~30s)")
	}
	if err := p.Join(channelID); err != nil {
		editResponse(e, fmt.Sprintf("Could not join: %v", err))
		return
	}
	editResponse(e, "Joined!")
}

func (m *MusicModule) handleLeave(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	if err := p.Disconnect(); err != nil {
		respondEphemeral(e, fmt.Sprintf("Error: %v", err))
		return
	}
	respond(e, "Left the voice channel.")
}

func (m *MusicModule) handleQueue(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	tracks := p.GetQueue()
	if len(tracks) == 0 {
		respond(e, "The queue is empty.")
		return
	}

	var sb strings.Builder
	sb.WriteString("**Queue:**\n")
	limit := len(tracks)
	if limit > 10 {
		limit = 10
	}
	for idx, t := range tracks[:limit] {
		fmt.Fprintf(&sb, "%d. %s\n", idx+1, t.Title)
	}
	if len(tracks) > 10 {
		fmt.Fprintf(&sb, "...and %d more", len(tracks)-10)
	}
	respond(e, sb.String())
}

func (m *MusicModule) handleNowPlaying(e *events.ApplicationCommandInteractionCreate) {
	guildID, ok := guildIDOf(e)
	if !ok {
		return
	}
	p := m.getOrCreatePlayer(guildID)
	t := p.NowPlaying()
	if t == nil {
		respond(e, "Nothing is playing.")
		return
	}
	respond(e, fmt.Sprintf("Now playing: **%s**", t.Title))
}
