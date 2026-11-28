package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/channels"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/prompts"
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
			if err := b.st.PutChannel(ctx, rec, rec.ChannelID); err != nil {
				return err
			}
			return b.st.PutCreating(ctx, store.Index{TaskID: rec.TaskID, WorkspaceID: rec.WorkspaceID, GuildID: rec.GuildID})
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
	if found && rec.Status == store.StatusFailed && rec.ChannelID != "" && rec.UpdatedAt.Before(b.accessRetryPoint(workspaceID)) {
		// A channel the bot lost access to is tried again once per
		// restart or successful provisioning (which every reconcile
		// repeats, and which fails while the bot is out of the guild). It
		// returns as active with unknown overwrites, so Converge rewrites
		// them before anything posts, and an open task resumes mirroring
		// while a closed or deleted one is archived or deleted.
		rec.Status, rec.LastError, rec.RolesKey, rec.UpdatedAt = store.StatusActive, "", "", b.now()
		if err := b.st.PutChannel(ctx, rec, rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", taskID, err)
			return
		}
	}
	mgr := b.channelManager()
	input := func(r store.Channel) channels.Input {
		parent, known := b.parentIdentifier(ctx, task)
		return channels.Input{Task: task, Rec: r, Guild: g, ParentIdentifier: parent, ParentUnknown: !known, Now: b.now()}
	}
	switch {
	case task != nil && rec.Status == store.StatusActive && !channels.WantOpen(b.cfg, task):
		var ok bool
		if rec, ok = b.flushBeforeClose(ctx, mgr, task, input(rec)); !ok {
			b.kickAfter(taskID, flushRetry)
			return
		}
	case !rec.CloseRequestedAt.IsZero() && channels.WantOpen(b.cfg, task):
		rec.CloseRequestedAt = time.Time{} // reopened before the close happened
		if err := b.st.PutChannel(ctx, rec, rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", taskID, err)
			return
		}
	}
	res, err := mgr.Sync(ctx, input(rec))
	if err != nil {
		log.Printf("discord: task %s channel: %v", taskID, err)
		if discordapi.IsForbidden(err) && res.Rec.Status == store.StatusActive && res.Rec.ChannelID != "" {
			b.lostAccess(ctx, res.Rec, rec.ChannelID, g, err)
			return
		}
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
		if creating(rec.Status) && res.Rec.Status != store.StatusCreating {
			if err := b.st.DeleteCreating(ctx, taskID); err != nil {
				log.Printf("discord: task %s: clearing the creation inventory: %v", taskID, err)
			}
		}
	}
	b.ensureIndexed(ctx, res.Rec)
	if res.RetryAfter > 0 {
		b.kickAfter(taskID, res.RetryAfter)
	}
	if res.Rec.Status == store.StatusArchived {
		// Pending prompts must not keep live buttons in a read-only channel;
		// ones whose edit fails are retried on the next sync.
		poster := &prompts.Poster{API: b.api, Host: b.host, Store: b.st, TaskURL: b.cfg.TaskURL, Now: b.now}
		if err := poster.Close(ctx, taskID, res.Rec.ChannelID); err != nil {
			log.Printf("discord: task %s: closing prompts: %v", taskID, err)
			b.kickAfter(taskID, flushRetry)
		}
	}
	if task == nil || res.Rec.Status != store.StatusActive || !channels.WantOpen(b.cfg, task) {
		return // an adopted channel of a closing task closes on the next sync
	}
	b.authority.RLock()
	defer b.authority.RUnlock()
	if cur, ok := b.guild(workspaceID); !ok || res.Rec.RolesKey != channels.RolesKey(cur) {
		b.kickAfter(taskID, flushRetry) // a former role may still read the channel
		return
	}
	_, err = b.syncActive(ctx, task, res.Rec)
	cur, found, rerr := b.st.Channel(ctx, taskID)
	if rerr != nil || !found || cur.ChannelID != res.Rec.ChannelID || cur.Status != store.StatusActive {
		return
	}
	switch {
	case discordapi.IsForbidden(err):
		b.lostAccess(ctx, cur, cur.ChannelID, g, err)
	case (err == nil || errors.Is(err, errSessionRunning)) && !cur.AccessLostAt.IsZero() && !b.needsCatchUp(cur.ChannelID):
		// Mirroring and catch-up work again: the outage is over.
		cur.AccessLostAt = time.Time{}
		if err := b.st.PutChannel(ctx, cur, cur.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", taskID, err)
			return
		}
		b.postStatus(ctx, g, fmt.Sprintf("🟢 The bot has access to <#%s> again; its task is mirrored again.", cur.ChannelID))
	}
}

// accessRetryPoint is when channels the bot lost access to are tried
// again: records failed before it are reactivated. It is the later of the
// process start and the workspace's last successful provisioning.
func (b *Bridge) accessRetryPoint(workspaceID string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.provisionedAt[workspaceID]; t.After(b.started) {
		return t
	}
	return b.started
}

// lostAccess records that Discord forbids the bot a channel (its overwrite
// or role permissions were removed, or the bot left the guild): the record
// becomes failed, so nothing is sent to the channel until the next retry
// point (accessRetryPoint). The channel must catch up before its next live
// message: messages posted meanwhile stay in its history. #kandev-status
// says so once per outage.
func (b *Bridge) lostAccess(ctx context.Context, rec store.Channel, previousChannelID string, g store.Guild, err error) {
	b.mu.Lock()
	b.caughtUp[rec.ChannelID] = -1
	b.mu.Unlock()
	first := rec.AccessLostAt.IsZero()
	if first {
		rec.AccessLostAt = b.now()
	}
	rec.Status, rec.LastError, rec.UpdatedAt = store.StatusFailed, "the bot lost access to the channel: "+err.Error(), b.now()
	if err := b.st.PutChannel(ctx, rec, previousChannelID); err != nil {
		log.Printf("discord: saving channel record of task %s: %v", rec.TaskID, err)
		return
	}
	if !first {
		return
	}
	log.Printf("discord: task %s: lost access to its channel; mirroring paused: %v", rec.TaskID, err)
	b.postStatus(ctx, g, fmt.Sprintf("⚠️ The bot lost access to <#%s>, so its task is not mirrored. Restore the bot's permissions there; the plugin retries every 5 minutes.", rec.ChannelID))
}

// closeHoldMax bounds how long a closing task's channel stays open waiting
// for a session that keeps running, from the first close attempt.
const closeHoldMax = 30 * time.Minute

// flushBeforeClose posts a closing task's final answer and status under
// converged permissions, fenced against role changes. It reports false
// when the close must wait: the overwrites or the flush failed
// transiently, or a session still runs (its last message may still grow),
// and the next sync retries. A flush Discord forbids (the bot lost access)
// cannot succeed later, and a session still running closeHoldMax after the
// first close attempt is not waited for longer, so the close goes ahead.
func (b *Bridge) flushBeforeClose(ctx context.Context, mgr *channels.Manager, task *pluginsdk.Task, in channels.Input) (store.Channel, bool) {
	if in.Rec.CloseRequestedAt.IsZero() {
		in.Rec.CloseRequestedAt = b.now()
		if err := b.st.PutChannel(ctx, in.Rec, in.Rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", task.ID, err)
			return in.Rec, false
		}
	}
	b.authority.RLock()
	defer b.authority.RUnlock()
	g, ok := b.guild(task.WorkspaceID)
	if !ok {
		return in.Rec, false
	}
	in.Guild = g
	res, err := mgr.Converge(ctx, in, false)
	if res.Changed {
		if err := b.st.PutChannel(ctx, res.Rec, in.Rec.ChannelID); err != nil {
			log.Printf("discord: saving channel record of task %s: %v", task.ID, err)
			return in.Rec, false
		}
	}
	if err != nil {
		if discordapi.IsForbidden(err) {
			// The bot lost access: nothing can be flushed or moved; stop.
			b.lostAccess(ctx, res.Rec, in.Rec.ChannelID, g, err)
			return res.Rec, false
		}
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
	case errors.Is(err, errSessionRunning) && b.now().Sub(next.CloseRequestedAt) > closeHoldMax:
		log.Printf("discord: task %s: closing although a session still runs", task.ID)
		return next, true
	}
	log.Printf("discord: task %s: holding the close until its final output posts: %v", task.ID, err)
	return next, false
}

// creating reports whether a record status leads to channel creation, the
// path that may have listed the task in the creation inventory.
func creating(status string) bool {
	switch status {
	case "", store.StatusDeleted, store.StatusCapacityBlocked, store.StatusCreating, store.StatusFailed:
		return true
	}
	return false
}

// ensureIndexed repairs a missing or stale reverse index entry of a
// channel the record owns (its write may have failed while the record's
// succeeded), then catches the channel up: messages posted while it was
// unroutable are still in its history.
func (b *Bridge) ensureIndexed(ctx context.Context, rec store.Channel) {
	if rec.ChannelID == "" || (rec.Status != store.StatusActive && rec.Status != store.StatusArchived) {
		return
	}
	ix, found, err := b.st.ChannelIndex(ctx, rec.ChannelID)
	if err != nil || (found && ix == store.Index{TaskID: rec.TaskID, WorkspaceID: rec.WorkspaceID, GuildID: rec.GuildID}) {
		return
	}
	if err := b.st.PutChannel(ctx, rec, rec.ChannelID); err != nil {
		log.Printf("discord: task %s: repairing the channel index: %v", rec.TaskID, err)
		return
	}
	if rec.Status == store.StatusActive {
		b.mu.Lock()
		b.caughtUp[rec.ChannelID] = -1
		b.mu.Unlock()
		b.catchUp(rec.TaskID, rec.ChannelID)
	}
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

// parentIdentifier names a subtask's parent for the channel topic. known is
// false when the parent could not be read (a Host error other than
// NotFound): the topic must not be rewritten from a guess.
func (b *Bridge) parentIdentifier(ctx context.Context, task *pluginsdk.Task) (id string, known bool) {
	if task == nil || task.ParentID == nil || *task.ParentID == "" {
		return "", true
	}
	parent, err := b.host.GetTask(ctx, *task.ParentID)
	if err != nil {
		return "", kandev.IsNotFound(err)
	}
	if parent.Identifier != "" {
		return parent.Identifier, true
	}
	return parent.Title, true
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
