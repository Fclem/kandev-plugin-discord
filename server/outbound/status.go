package outbound

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
)

// Running reports a session state with a live turn.
func Running(state string) bool { return state == "RUNNING" || state == "STARTING" }

// StatusLine is one session's transition announcement.
type StatusLine struct {
	SessionID string
	Text      string
}

// StatusLines compares observed session states with the last recorded ones
// and returns at most one line per session for a meaningful transition,
// plus the new record. A session seen for the first time only produces a
// line when it is running.
func StatusLines(prev map[string]string, sessions []pluginsdk.Session) ([]StatusLine, map[string]string) {
	sorted := append([]pluginsdk.Session(nil), sessions...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartedAt < sorted[j].StartedAt })
	next := map[string]string{}
	var lines []StatusLine
	for _, s := range sorted {
		next[s.ID] = s.State
		old, seen := prev[s.ID]
		if old == s.State {
			continue
		}
		who := agentName(s)
		text := ""
		switch {
		case Running(s.State):
			if !Running(old) {
				text = fmt.Sprintf("▶️ %s is working.", who)
			}
		case !seen:
		case s.State == "WAITING_FOR_INPUT" || s.State == "IDLE":
			if Running(old) {
				text = fmt.Sprintf("✅ %s finished its turn and is waiting for input.", who)
			}
		case s.State == "COMPLETED":
			text = fmt.Sprintf("🏁 %s completed the session.", who)
		case s.State == "FAILED":
			text = fmt.Sprintf("❌ %s failed.", who)
		case s.State == "CANCELLED":
			text = fmt.Sprintf("⏹️ %s was stopped.", who)
		}
		if text != "" {
			lines = append(lines, StatusLine{SessionID: s.ID, Text: text})
		}
	}
	return lines, next
}

func agentName(s pluginsdk.Session) string {
	for _, n := range []string{s.AgentDisplayName, s.AgentProfileName} {
		if strings.TrimSpace(n) != "" {
			return "**" + strings.TrimSpace(n) + "**"
		}
	}
	return "The agent"
}

// CardInput is what the pinned task card shows.
type CardInput struct {
	Task     *pluginsdk.Task
	StepName string
	Sessions []pluginsdk.Session
	Queued   int
	Pending  int
	URL      string
}

// Card renders the task card and a hash of its content.
func Card(in CardInput) (*discordgo.MessageEmbed, string) {
	title := in.Task.Title
	if in.Task.Identifier != "" {
		title = in.Task.Identifier + " · " + title
	}
	if r := []rune(title); len(r) > 256 {
		title = string(r[:255]) + "…"
	}
	step := in.StepName
	if step == "" {
		step = "—"
	}
	sessions := "none yet"
	if len(in.Sessions) > 0 {
		var parts []string
		for _, s := range in.Sessions {
			parts = append(parts, fmt.Sprintf("%s: %s", strings.Trim(agentName(s), "*"), strings.ToLower(s.State)))
		}
		sessions = strings.Join(parts, "\n")
		if r := []rune(sessions); len(r) > 1024 {
			sessions = string(r[:1023]) + "…"
		}
	}
	e := &discordgo.MessageEmbed{
		Title: title, URL: in.URL,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "State", Value: strings.ToLower(strings.ReplaceAll(in.Task.State, "_", " ")), Inline: true},
			{Name: "Step", Value: step, Inline: true},
			{Name: "Queued messages", Value: fmt.Sprint(in.Queued), Inline: true},
			{Name: "Pending requests", Value: fmt.Sprint(in.Pending), Inline: true},
			{Name: "Sessions", Value: sessions},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "Kandev task card · updated by the Discord bridge"},
	}
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return e, hex.EncodeToString(sum[:8])
}
