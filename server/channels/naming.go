// Package channels owns the task channel lifecycle: naming, creation with
// category rollover, rename debounce, archive or delete, reopen, and
// retention (docs/design/system-design.md "State machine (task channel)").
package channels

import (
	"strings"
	"unicode"

	"github.com/kdlbs/kandev/pluginsdk"
	"golang.org/x/text/unicode/norm"
)

// MaxNameLength is Discord's channel name limit.
const MaxNameLength = 100

// maxTopicLength is Discord's text channel topic limit.
const maxTopicLength = 1024

// Name derives `<identifier>-<slug(title)>`: lowercase ASCII letters,
// digits, and single dashes, at most 100 characters.
func Name(task *pluginsdk.Task) string {
	prefix := slug(task.Identifier)
	if prefix == "" {
		id := strings.ReplaceAll(task.ID, "-", "")
		prefix = "task-" + slug(id[:min(8, len(id))])
	}
	name := prefix
	if title := slug(task.Title); title != "" {
		name += "-" + title
	}
	if len(name) > MaxNameLength {
		name = strings.TrimRight(name[:MaxNameLength], "-")
	}
	return name
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFKD.String(strings.ToLower(s)) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case unicode.Is(unicode.Mn, r):
			// drop combining marks left by NFKD (é → e)
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// Topic renders the channel topic: title, identifier, parent, and link.
func Topic(task *pluginsdk.Task, parentIdentifier, url string) string {
	parts := []string{strings.TrimSpace(task.Title)}
	if task.Identifier != "" {
		parts = append(parts, task.Identifier)
	}
	if parentIdentifier != "" {
		parts = append(parts, "subtask of "+parentIdentifier)
	}
	if url != "" {
		parts = append(parts, url)
	}
	topic := strings.Join(parts, " · ")
	if r := []rune(topic); len(r) > maxTopicLength {
		topic = string(r[:maxTopicLength-1]) + "…"
	}
	return topic
}
