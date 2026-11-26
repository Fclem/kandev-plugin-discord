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

// reconcileAll reconciles every provisioned workspace and retries
// provisioning of bound guilds that failed or ran before READY.
func (b *Bridge) reconcileAll() {
	for _, binding := range b.cfg.Bindings {
		if _, ok := b.guild(binding.WorkspaceID); ok {
			b.sched.Do("guild:"+binding.GuildID, func(ctx context.Context) { b.reconcileWorkspace(ctx, binding, false) })
			continue
		}
		if b.isMember(binding.WorkspaceID) && b.botID() != "" {
			b.sched.Do("guild:"+binding.GuildID, func(ctx context.Context) { b.provisionAndReconcile(ctx, binding) })
		}
	}
}

// reconcileWorkspace queues a sync for every task that has, or should have,
// a channel, and for every indexed channel whose task vanished. Channels
// missing from the guild (deleted while the bridge was offline) are marked
// deleted first, so the sync recreates them. Syncs are idempotent, so an
// unchanged workspace causes no Discord mutation. After a READY it also
// queues the inbound catch-up of each channel, after its sync.
func (b *Bridge) reconcileWorkspace(ctx context.Context, binding config.Binding, afterReady bool) {
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
		if afterReady && ix.GuildID == binding.GuildID {
			b.catchUp(ix.TaskID, channelID)
		}
	}
}
