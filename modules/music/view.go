package music

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"
)

// Custom IDs of the player panel buttons.
const (
	btnPauseResume = "player_pause_resume"
	btnSkip        = "player_skip"
	btnLoop        = "player_loop"
	btnQueue       = "player_queue"
	btnStop        = "player_stop"
)

const (
	colorBlurple = 0x5865F2
	colorGrey    = 0x99AAB5
	colorRed     = 0xED4245
)

func fmtDuration(sec int) string {
	if sec <= 0 {
		return "?"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func boolField(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

// playerEmbed renders the panel embed. tr == nil renders the idle variant.
func playerEmbed(tr *Track, paused, looping bool) discord.Embed {
	if tr == nil {
		return discord.Embed{Title: "Nothing playing", Color: colorGrey}
	}
	inline := true
	return discord.Embed{
		Title: tr.Title,
		URL:   tr.WebpageURL,
		Color: colorBlurple,
		Fields: []discord.EmbedField{
			{Name: "State", Value: boolField(paused, "❚❚ Paused", "▶ Playing"), Inline: &inline},
			{Name: "Duration", Value: fmtDuration(tr.Duration), Inline: &inline},
			{Name: "Loop", Value: boolField(looping, "On", "Off"), Inline: &inline},
		},
	}
}

func stoppedEmbed() discord.Embed {
	return discord.Embed{Title: "Stopped", Color: colorRed}
}

// playerButtons renders the panel's action row reflecting current state.
func playerButtons(paused, looping, disabled bool) []discord.LayoutComponent {
	pauseLabel := "⏸"
	if paused {
		pauseLabel = "▶"
	}
	loopBtn := discord.NewSecondaryButton("↺", btnLoop)
	if looping {
		loopBtn = discord.NewSuccessButton("↺", btnLoop)
	}
	row := discord.NewActionRow(
		discord.NewPrimaryButton(pauseLabel, btnPauseResume).WithDisabled(disabled),
		discord.NewSecondaryButton("⏭", btnSkip).WithDisabled(disabled),
		loopBtn.WithDisabled(disabled),
		discord.NewSecondaryButton("☰", btnQueue).WithDisabled(disabled),
		discord.NewDangerButton("⏹", btnStop).WithDisabled(disabled),
	)
	return []discord.LayoutComponent{row}
}
