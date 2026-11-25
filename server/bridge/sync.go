package bridge

import (
	"context"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/channels"
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
		SaveGuild: b.setGuild, Note: b.postStatus,
	}
}

// syncTask converges one task: channel lifecycle, then (for an active
// channel) outbound mirroring and prompts. It runs on the task's queue.
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
	res, err := b.channelManager().Sync(ctx, channels.Input{
		Task: task, Rec: rec, Guild: g, ParentIdentifier: b.parentIdentifier(ctx, task), Now: b.now(),
	})
	if err != nil {
		log.Printf("discord: task %s channel: %v", taskID, err)
	}
	if res.Changed {
		if err := b.st.PutChannel(ctx, res.Rec, rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", taskID, err)
			return
		}
	}
	if res.RetryAfter > 0 {
		b.kickAfter(taskID, res.RetryAfter)
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
func (b *Bridge) onChannelDelete(ctx context.Context, ch *discordgo.Channel) {
	ix, ok, err := b.st.ChannelIndex(ctx, ch.ID)
	if err != nil || !ok {
		return
	}
	b.sched.Do(ix.TaskID, func(ctx context.Context) {
		rec, found, err := b.st.Channel(ctx, ix.TaskID)
		if err != nil || !found || rec.ChannelID != ch.ID || rec.Status == store.StatusDeleted {
			return
		}
		if err := b.st.PutChannel(ctx, channels.MarkDeleted(rec, b.now()), rec.ChannelID); err != nil {
			log.Printf("discord: saving deleted channel of task %s: %v", ix.TaskID, err)
		}
	})
}
