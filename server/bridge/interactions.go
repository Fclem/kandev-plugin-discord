package bridge

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/commands"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/store"
)

func (b *Bridge) router() *commands.Router {
	return &commands.Router{Env: commands.Env{
		API: b.api, Host: b.host, Config: b.cfg, Store: b.st,
		Guild:      func(guildID string) (store.Guild, config.Binding, bool) { return b.guildByID(guildID) },
		QueueCount: b.queueCount,
		StepName:   b.stepName,
		OnTask:     b.onTaskAndWait,
		Refresh:    b.Kick,
	}}
}

// onInteraction answers slash commands, buttons, and autocomplete.
func (b *Bridge) onInteraction(ctx context.Context, i *discordgo.InteractionCreate) {
	if i == nil || i.Interaction == nil || i.GuildID == "" || !b.events.add("ix:"+i.ID) {
		return
	}
	b.router().Handle(ctx, i.Interaction)
}

// onTaskAndWait runs fn on the task's queue and waits for it, so an
// approval and the task's sync never write the same records concurrently.
func (b *Bridge) onTaskAndWait(taskID string, fn func(ctx context.Context)) {
	done := make(chan struct{})
	b.sched.Do(taskID, func(ctx context.Context) {
		defer close(done)
		fn(ctx)
	})
	<-done
}
