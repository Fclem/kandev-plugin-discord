package outbound

import (
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"
)

// PluginSource is the Message.Source Kandev stamps on messages this plugin
// sent with Messages().Send; they are never echoed back.
const PluginSource = "plugin:kandev-plugin-discord"

// Options selects which messages are mirrored.
type Options struct {
	MirrorUserMessages bool
	PostToolActivity   bool
}

const toolSummaryLength = 200

// Render returns the Discord text for one Kandev message, or false when the
// message is not mirrored. Thinking is never mirrored.
func Render(m pluginsdk.Message, o Options) (string, bool) {
	content := strings.TrimSpace(m.Content)
	if content == "" {
		return "", false
	}
	switch {
	case m.Type == "thinking":
		return "", false
	case m.AuthorType == "agent" && (m.Type == "message" || m.Type == "content" || m.Type == ""):
		return content, true
	case m.AuthorType == "user" && (m.Type == "message" || m.Type == ""):
		if !o.MirrorUserMessages || m.Source == PluginSource {
			return "", false
		}
		return "**Kandev:** " + content, true
	case strings.HasPrefix(m.Type, "tool_") && o.PostToolActivity:
		line, _, _ := strings.Cut(content, "\n")
		if r := []rune(line); len(r) > toolSummaryLength {
			line = string(r[:toolSummaryLength]) + "…"
		}
		return "🔧 `" + strings.ReplaceAll(line, "`", "'") + "`", true
	}
	return "", false
}
