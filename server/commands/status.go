package commands

import (
	"context"
	"fmt"
	"strings"

	"kandev-plugin-discord/server/kandev"
)

// /status stays well under Discord's 2000 characters: at most this many
// sessions are listed, and long names are shortened.
const (
	maxStatusSessions = 10
	maxStatusName     = 60
	maxStatusTitle    = 200
)

// clipLine shortens one line to n characters with an ellipsis.
func clipLine(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// status replies with the task's state; anyone who sees the channel may ask.
func (c *call) status(ctx context.Context, taskID string) {
	task, err := c.Host.GetTask(ctx, taskID)
	if err != nil {
		if kandev.IsNotFound(err) {
			c.reply(ctx, "This task no longer exists in Kandev.")
			return
		}
		c.reply(ctx, "Kandev could not read the task right now; try again.")
		return
	}
	var b strings.Builder
	title := task.Title
	if task.Identifier != "" {
		title = task.Identifier + " · " + title
	}
	fmt.Fprintf(&b, "**%s**\n", clipLine(title, maxStatusTitle))
	state := strings.ToLower(strings.ReplaceAll(task.State, "_", " "))
	if task.ArchivedAt != nil {
		state += " (archived)"
	}
	fmt.Fprintf(&b, "State: %s", state)
	if step := c.StepName(ctx, task); step != "" {
		fmt.Fprintf(&b, " · Step: %s", step)
	}
	b.WriteString("\n")

	sessions, err := c.Host.ListSessions(ctx, taskID)
	queued := 0
	switch {
	case err != nil:
		b.WriteString("Sessions: unavailable\n")
	case len(sessions) == 0:
		b.WriteString("Sessions: none yet\n")
	default:
		b.WriteString("Sessions:\n")
		for i, s := range sessions {
			queued += c.QueueCount(s.ID)
			if i == maxStatusSessions {
				fmt.Fprintf(&b, "• … and %d more\n", len(sessions)-maxStatusSessions)
			}
			if i >= maxStatusSessions {
				continue
			}
			name := s.AgentDisplayName
			if name == "" {
				name = s.AgentProfileName
			}
			if name == "" {
				name = "agent"
			}
			fmt.Fprintf(&b, "• %s — %s\n", clipLine(name, maxStatusName), strings.ToLower(strings.ReplaceAll(s.State, "_", " ")))
		}
	}
	pending := "unavailable"
	if items, err := c.Host.ListPendingInteractions(ctx, taskID); err == nil {
		pending = fmt.Sprint(len(items))
	}
	fmt.Fprintf(&b, "Queued messages: %d · Pending requests: %s", queued, pending)
	if url := c.Config.TaskURL(taskID); url != "" {
		fmt.Fprintf(&b, "\n%s", url)
	}
	c.reply(ctx, b.String())
}
