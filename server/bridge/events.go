package bridge

import (
	"context"
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"
)

// HandleEvent turns a Kandev event into a task sync. Events are triggers
// only: content comes from Host reads, so a dropped or replayed event costs
// at most one reconcile interval.
func (b *Bridge) HandleEvent(_ context.Context, e *pluginsdk.Event) {
	if e == nil || !b.events.add(e.EventID) {
		return
	}
	taskID := payloadString(e.Payload, "task_id", "taskId")
	sessionID := payloadString(e.Payload, "session_id", "sessionId")
	if e.EventType == "message.queue.status_changed" && sessionID != "" {
		if n, ok := e.Payload["count"].(float64); ok {
			b.setQueueCount(sessionID, int(n))
		}
	}
	if taskID == "" && sessionID != "" {
		taskID = b.taskOfSession(sessionID)
	}
	if taskID == "" && strings.HasPrefix(e.EventType, "task.") {
		taskID = payloadString(e.Payload, "id")
	}
	b.Kick(taskID)
}

func payloadString(p map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := p[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// setQueueCount caches a session's queue depth for /status and the card.
func (b *Bridge) setQueueCount(sessionID string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queueCounts == nil {
		b.queueCounts = map[string]int{}
	}
	b.queueCounts[sessionID] = n
}

func (b *Bridge) queueCount(sessionID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queueCounts[sessionID]
}

// rememberSessions maps sessions to their task for events that carry only
// a session ID.
func (b *Bridge) rememberSessions(taskID string, sessions []pluginsdk.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionTasks == nil {
		b.sessionTasks = map[string]string{}
	}
	for _, s := range sessions {
		b.sessionTasks[s.ID] = taskID
	}
}

func (b *Bridge) taskOfSession(sessionID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionTasks[sessionID]
}
