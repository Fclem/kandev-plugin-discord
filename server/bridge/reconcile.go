package bridge

import (
	"context"
	"log"
	"time"

	"kandev-plugin-discord/server/channels"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/store"
)

// ReconcileInterval is how often every bound workspace is converged.
const ReconcileInterval = 5 * time.Minute

// reconcileLoop reconciles every provisioned workspace periodically.
func (b *Bridge) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reconcileAll()
		}
	}
}

// reconcileAll provisions (idempotently: it repairs deleted categories,
// roles, and #kandev-status) and reconciles every bound guild the bot is in
// once its identity is known.
func (b *Bridge) reconcileAll() {
	for _, binding := range b.cfg.Bindings {
		_, known := b.guild(binding.WorkspaceID)
		if known || (b.isMember(binding.WorkspaceID) && b.botID() != "") {
			b.sched.Do("guild:"+binding.GuildID, func(ctx context.Context) { b.provisionAndReconcile(ctx, binding) })
		}
	}
}

// reconcileWorkspace queues a sync for every task that has, or should have,
// a channel, and for every indexed channel whose task vanished. Channels
// missing from the guild (deleted while the bridge was offline) are marked
// deleted first, so the sync recreates them. Syncs are idempotent, so an
// unchanged workspace causes no Discord mutation. Channels that have not
// caught up since the last READY (or since a failed delivery) get their
// inbound catch-up queued, after their sync.
func (b *Bridge) reconcileWorkspace(ctx context.Context, binding config.Binding) {
	tasks, err := b.host.ListTasks(ctx, binding.WorkspaceID, b.cfg.IncludeEphemeralTasks)
	if err != nil {
		log.Printf("discord: reconcile workspace %s: listing tasks: %v", binding.WorkspaceID, err)
		return
	}
	index, err := b.st.IndexedChannels(ctx)
	if err != nil {
		log.Printf("discord: reconcile workspace %s: reading channel index: %v", binding.WorkspaceID, err)
		return
	}
	inventory, err := b.api.GuildChannels(ctx, binding.GuildID)
	if err != nil {
		log.Printf("discord: reconcile workspace %s: listing channels: %v", binding.WorkspaceID, err)
		return
	}
	exists := map[string]bool{}
	for _, ch := range inventory {
		exists[ch.ID] = true
	}
	for channelID, ix := range index {
		if ix.WorkspaceID == binding.WorkspaceID && ix.GuildID == binding.GuildID && !exists[channelID] {
			b.onTaskAndWait(ix.TaskID, func(ctx context.Context) { b.markGone(ctx, ix.TaskID, channelID) })
			delete(index, channelID)
		}
	}
	seen := map[string]bool{}
	for i := range tasks {
		task := &tasks[i]
		seen[task.ID] = true
		if channels.WantOpen(b.cfg, task) {
			b.Kick(task.ID)
			continue
		}
		rec, found, err := b.st.Channel(ctx, task.ID)
		if err == nil && found && rec.Status != store.StatusDeleted {
			b.Kick(task.ID)
		}
	}
	for channelID, ix := range index {
		if ix.WorkspaceID != binding.WorkspaceID {
			continue
		}
		if !seen[ix.TaskID] || ix.GuildID != binding.GuildID {
			b.Kick(ix.TaskID) // task gone, or a channel of a former binding
		}
		if ix.GuildID == binding.GuildID && b.needsCatchUp(channelID) {
			b.catchUp(ix.TaskID, channelID)
		}
	}
	b.reconcileCreations(ctx, binding, seen)
}

// reconcileCreations syncs tasks whose channel creation is unrecorded but
// that the task list no longer shows (deleted while the bridge was down),
// so a channel Discord made for them is closed, and drops inventory
// entries whose creation has settled.
func (b *Bridge) reconcileCreations(ctx context.Context, binding config.Binding, seen map[string]bool) {
	pending, err := b.st.CreatingTasks(ctx)
	if err != nil {
		log.Printf("discord: reconcile workspace %s: reading the creation inventory: %v", binding.WorkspaceID, err)
		return
	}
	for taskID, ix := range pending {
		if ix.WorkspaceID != binding.WorkspaceID || seen[taskID] {
			continue // listed tasks are synced above
		}
		rec, found, err := b.st.Channel(ctx, taskID)
		switch {
		case err != nil:
			continue
		case found && rec.Status == store.StatusCreating:
			b.Kick(taskID)
		default:
			if err := b.st.DeleteCreating(ctx, taskID); err != nil {
				log.Printf("discord: task %s: clearing the creation inventory: %v", taskID, err)
			}
		}
	}
}
