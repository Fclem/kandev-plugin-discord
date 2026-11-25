package bridge

import (
	"context"
	"log"
	"reflect"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/outbound"
	"kandev-plugin-discord/server/prompts"
	"kandev-plugin-discord/server/store"
)

// stepCacheTTL bounds how long workflow step names are cached.
const stepCacheTTL = 5 * time.Minute

type stepCache struct {
	steps   []pluginsdk.WorkflowStep
	fetched time.Time
}

// syncActive mirrors an active channel: messages, status lines, the card,
// and pending prompts. It schedules the next live update while a session
// runs.
func (b *Bridge) syncActive(ctx context.Context, task *pluginsdk.Task, rec store.Channel) {
	sessions, err := b.host.ListSessions(ctx, task.ID)
	if err != nil {
		log.Printf("discord: task %s: listing sessions: %v", task.ID, err)
		return
	}
	b.rememberSessions(task.ID, sessions)
	pending := b.pendingCount(ctx, task.ID)
	mirror := &outbound.Mirror{
		API: b.api, Host: b.host, TaskURL: b.cfg.TaskURL,
		Opts: outbound.Options{MirrorUserMessages: b.cfg.MirrorUserMessages, PostToolActivity: b.cfg.PostToolActivity},
		Save: func(ctx context.Context, r store.Channel) error { return b.st.PutChannel(ctx, r, r.ChannelID) },
	}
	next, retry, err := mirror.Sync(ctx, outbound.Input{
		Task: task, Rec: rec, Sessions: sessions, Now: b.now(),
		Card: outbound.CardInput{
			Task: task, StepName: b.stepName(ctx, task), Sessions: sessions,
			Queued: b.queued(sessions), Pending: pending, URL: b.cfg.TaskURL(task.ID),
		},
	})
	if err != nil {
		log.Printf("discord: task %s: mirroring: %v", task.ID, err)
	}
	if !reflect.DeepEqual(next, rec) {
		if err := b.st.PutChannel(ctx, next, next.ChannelID); err != nil {
			log.Printf("discord: task %s: saving channel record: %v", task.ID, err)
		}
	}
	if retry > 0 {
		b.kickAfter(task.ID, retry)
	}
	if next.Status == store.StatusActive {
		poster := &prompts.Poster{API: b.api, Host: b.host, Store: b.st, TaskURL: b.cfg.TaskURL, Now: b.now}
		if err := poster.Sync(ctx, task, next, sessions); err != nil {
			log.Printf("discord: task %s: prompts: %v", task.ID, err)
		}
	}
	if b.cfg.LiveUpdateInterval > 0 && anyRunning(sessions) {
		b.kickAfter(task.ID, b.cfg.LiveUpdateInterval)
	}
}

func anyRunning(sessions []pluginsdk.Session) bool {
	for _, s := range sessions {
		if outbound.Running(s.State) {
			return true
		}
	}
	return false
}

func (b *Bridge) queued(sessions []pluginsdk.Session) int {
	n := 0
	for _, s := range sessions {
		n += b.queueCount(s.ID)
	}
	return n
}

// pendingCount is the number of pending interactions, or 0 when the host
// cannot say.
func (b *Bridge) pendingCount(ctx context.Context, taskID string) int {
	pending, err := b.host.ListPendingInteractions(ctx, taskID)
	if err != nil {
		return 0
	}
	return len(pending)
}

// stepName resolves the task's workflow step name through a short cache.
func (b *Bridge) stepName(ctx context.Context, task *pluginsdk.Task) string {
	if task.WorkflowID == "" || task.WorkflowStepID == "" {
		return ""
	}
	b.mu.Lock()
	cached, ok := b.steps[task.WorkflowID]
	b.mu.Unlock()
	if !ok || b.now().Sub(cached.fetched) > stepCacheTTL {
		steps, err := b.host.ListWorkflowSteps(ctx, task.WorkflowID)
		if err != nil {
			return ""
		}
		cached = stepCache{steps: steps, fetched: b.now()}
		b.mu.Lock()
		if b.steps == nil {
			b.steps = map[string]stepCache{}
		}
		b.steps[task.WorkflowID] = cached
		b.mu.Unlock()
	}
	for _, s := range cached.steps {
		if s.ID == task.WorkflowStepID {
			return s.Name
		}
	}
	return ""
}
