package bridge

import (
	"context"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/channels"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

// Kick queues one sync of a task; kicks that arrive while a sync is queued
// coalesce into it.
func (b *Bridge) Kick(taskID string) {
	if taskID == "" {
		return
	}
	b.mu.Lock()
	if b.syncing[taskID] {
		b.mu.Unlock()
		return
	}
	b.syncing[taskID] = true
	b.mu.Unlock()
	b.sched.Do(taskID, func(ctx context.Context) {
		b.mu.Lock()
		delete(b.syncing, taskID)
		b.mu.Unlock()
		b.syncTask(ctx, taskID)
	})
}

// kickAfter syncs a task again after d (rename debounce, retention, card
// debounce, live updates). An earlier pending kick wins.
func (b *Bridge) kickAfter(taskID string, d time.Duration) {
	b.timers.after("sync:"+taskID, d, func() { b.Kick(taskID) })
}

func (b *Bridge) channelManager() *channels.Manager {
	return &channels.Manager{
		API: b.api, Config: b.cfg, BotUserID: b.botID(), StartedAt: b.started,
		CategoryLock: &b.categoryMu, CurrentGuild: b.guild,
		SaveGuild: b.setGuild, Note: b.postStatus,
		SaveIntent: func(ctx context.Context, rec store.Channel) error {
			return b.st.PutChannel(ctx, rec, rec.ChannelID)
		},
		Owner: func(ctx context.Context, channelID string) (string, bool, error) {
			ix, found, err := b.st.ChannelIndex(ctx, channelID)
			return ix.TaskID, found, err
		},
	}
}

// flushRetry is how soon a task whose outbound could not be flushed safely
// (permissions not converged, or Discord failed) is synced again.
const flushRetry = time.Minute

// syncTask converges one task: channel lifecycle, then (for an active
// channel) outbound mirroring and prompts. It runs on the task's queue.
// Nothing is posted to a channel whose overwrites are not yet those of the
// current roles, and a channel only closes once its final output posted.
func (b *Bridge) syncTask(ctx context.Context, taskID string) {
	task, err := b.host.GetTask(ctx, taskID)
	if err != nil && !kandev.IsNotFound(err) {
		log.Printf("discord: reading task %s: %v", taskID, err)
		return
	}
	if err != nil {
		task = nil
	}
	rec, found, err := b.st.Channel(ctx, taskID)
	if err != nil {
		log.Printf("discord: reading channel record of task %s: %v", taskID, err)
		return
	}
	if !found && task == nil {
		return
	}
	workspaceID := rec.WorkspaceID
	if task != nil {
		workspaceID = task.WorkspaceID
	}
	g, ok := b.guild(workspaceID)
	if !ok {
		return // unbound workspace, or provisioning has not finished
	}
	if found && rec.GuildID != "" && rec.GuildID != g.GuildID {
		if rec, err = b.retire(ctx, rec); err != nil {
			log.Printf("discord: task %s: retiring channel of a former binding: %v", taskID, err)
			return
		}
	}
	mgr := b.channelManager()
	input := func(r store.Channel) channels.Input {
		return channels.Input{Task: task, Rec: r, Guild: g, ParentIdentifier: b.parentIdentifier(ctx, task), Now: b.now()}
	}
	if task != nil && rec.Status == store.StatusActive && !channels.WantOpen(b.cfg, task) {
		var ok bool
		if rec, ok = b.flushBeforeClose(ctx, mgr, task, input(rec)); !ok {
			b.kickAfter(taskID, flushRetry)
			return
		}
	}
	res, err := mgr.Sync(ctx, input(rec))
	if err != nil {
		log.Printf("discord: task %s channel: %v", taskID, err)
	}
	if res.Changed {
		if err := b.st.PutChannel(ctx, res.Rec, rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", taskID, err)
			return
		}
		if res.Rec.ChannelID != rec.ChannelID && res.Rec.Status == store.StatusActive {
			// A new channel: prompts posted in the old one are unreachable.
			b.forgetPrompts(ctx, taskID)
		}
	}
	if res.RetryAfter > 0 {
		b.kickAfter(taskID, res.RetryAfter)
	}
	if task == nil || res.Rec.Status != store.StatusActive {
		return
	}
	if res.Rec.RolesKey != channels.RolesKey(g) {
		b.kickAfter(taskID, flushRetry) // a former role may still read the channel
		return
	}
	b.syncActive(ctx, task, res.Rec)
}

// flushBeforeClose posts a closing task's final answer and status under
// converged permissions. It reports false when the close must wait: the
// overwrites or the flush failed transiently, and the next sync retries
// both. A flush Discord forbids (the bot lost access) cannot succeed later,
// so the close goes ahead.
func (b *Bridge) flushBeforeClose(ctx context.Context, mgr *channels.Manager, task *pluginsdk.Task, in channels.Input) (store.Channel, bool) {
	res, err := mgr.Converge(ctx, in, false)
	if res.Changed {
		if err := b.st.PutChannel(ctx, res.Rec, in.Rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", task.ID, err)
			return in.Rec, false
		}
	}
	if err != nil {
		log.Printf("discord: task %s: holding the close until permissions update: %v", task.ID, err)
		return res.Rec, false
	}
	if res.Rec.Status != store.StatusActive {
		return res.Rec, true // deleted in Discord: nothing left to flush
	}
	next, err := b.syncActive(ctx, task, res.Rec)
	switch {
	case err == nil:
		return next, true
	case discordapi.IsForbidden(err):
		log.Printf("discord: task %s: closing without its final output: %v", task.ID, err)
		return next, true
	}
	log.Printf("discord: task %s: holding the close until its final output posts: %v", task.ID, err)
	return next, false
}

// retire detaches a record from a guild the workspace is no longer bound
// to: its channel leaves the reverse index (no more traffic either way) and
// the task gets a fresh channel in the new guild. The outbound cursor is
// kept, so the history already mirrored is not posted again.
func (b *Bridge) retire(ctx context.Context, rec store.Channel) (store.Channel, error) {
	old := rec
	old.Status = store.StatusDeleted
	if err := b.st.PutChannel(ctx, old, old.ChannelID); err != nil {
		return rec, err
	}
	b.forgetPrompts(ctx, rec.TaskID)
	log.Printf("discord: task %s moved from server %s to a new binding; creating a new channel", rec.TaskID, rec.GuildID)
	return store.Channel{TaskID: rec.TaskID, WorkspaceID: rec.WorkspaceID, OutboundCursor: rec.OutboundCursor}, nil
}

func (b *Bridge) forgetPrompts(ctx context.Context, taskID string) {
	if err := b.st.PutPrompts(ctx, taskID, store.Prompts{}); err != nil {
		log.Printf("discord: task %s: clearing prompts: %v", taskID, err)
	}
	if err := b.st.DeleteStepApproval(ctx, taskID); err != nil {
		log.Printf("discord: task %s: clearing step prompt: %v", taskID, err)
	}
}

func (b *Bridge) parentIdentifier(ctx context.Context, task *pluginsdk.Task) string {
	if task == nil || task.ParentID == nil || *task.ParentID == "" {
		return ""
	}
	parent, err := b.host.GetTask(ctx, *task.ParentID)
	if err != nil {
		return ""
	}
	if parent.Identifier != "" {
		return parent.Identifier
	}
	return parent.Title
}

// onChannelDelete marks a task channel deleted when Discord deletes it; the
// next reconcile recreates it if the task is still open.
func (b *Bridge) onChannelDelete(_ context.Context, ch *discordgo.Channel) {
	b.sched.Do("chan:"+ch.ID, func(ctx context.Context) {
		ix, ok, err := b.st.ChannelIndex(ctx, ch.ID)
		if err != nil || !ok {
			return
		}
		b.onTaskAndWait(ix.TaskID, func(ctx context.Context) { b.markGone(ctx, ix.TaskID, ch.ID) })
	})
}

// markGone records that Discord no longer has a task's channel.
func (b *Bridge) markGone(ctx context.Context, taskID, channelID string) bool {
	rec, found, err := b.st.Channel(ctx, taskID)
	if err != nil || !found || rec.ChannelID != channelID || rec.Status == store.StatusDeleted {
		return false
	}
	if err := b.st.PutChannel(ctx, channels.MarkDeleted(rec, b.now()), rec.ChannelID); err != nil {
		log.Printf("discord: saving deleted channel of task %s: %v", taskID, err)
		return false
	}
	return true
}
