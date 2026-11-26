package bridge

import (
	"context"
	"fmt"
	"log"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/gateway"
	"kandev-plugin-discord/server/store"
)

// Lifecycle notices in #kandev-status: one per meaningful per-guild
// transition (connected, provisioning failed, disconnected, reconnected),
// never one per routine gateway reconnect.
const (
	noticeUp     = "up"
	noticeDown   = "down"
	noticeFailed = "failed"
)

// noticeUp records a successful provisioning and announces it when it ends
// a first start, an outage, or a provisioning failure.
func (b *Bridge) noticeUp(ctx context.Context, binding config.Binding, g store.Guild) {
	b.mu.Lock()
	prev := b.notices[g.GuildID]
	b.notices[g.GuildID] = noticeUp
	b.mu.Unlock()
	var text string
	switch prev {
	case noticeUp:
		return
	case noticeDown:
		text = "🟢 Reconnected to Discord; Kandev updates resume."
	case noticeFailed:
		text = fmt.Sprintf("🟢 Provisioning succeeded; this server mirrors Kandev workspace `%s` again.", binding.WorkspaceID)
	default:
		text = fmt.Sprintf("🟢 Connected: this server mirrors Kandev workspace `%s`.", binding.WorkspaceID)
	}
	log.Printf("discord: server %s provisioned for workspace %s", binding.GuildID, binding.WorkspaceID)
	b.postStatus(ctx, g, text)
}

// noticeProvisioningFailed announces the first failure of a streak, in the
// #kandev-status of the last good record (if any).
func (b *Bridge) noticeProvisioningFailed(ctx context.Context, binding config.Binding, err error) {
	if b.botID() == "" {
		return // READY has not arrived; provisioning simply waits for it
	}
	b.mu.Lock()
	prev := b.notices[binding.GuildID]
	b.notices[binding.GuildID] = noticeFailed
	b.mu.Unlock()
	if prev == noticeFailed {
		return
	}
	g, ok := b.guild(binding.WorkspaceID)
	if !ok {
		stored, found, err := b.st.Guild(ctx, binding.WorkspaceID)
		if err != nil || !found || stored.GuildID != binding.GuildID {
			return
		}
		g = stored
	}
	b.postStatus(ctx, g, fmt.Sprintf("⚠️ Provisioning failed: %v. The plugin retries every 5 minutes.", err))
}

// gatewayChanged announces outages: a stop at once, a disconnect only when
// the gateway has not come back within downNoticeDelay. A token rejection
// (HTTP 401) also disables REST, so nothing can be posted.
func (b *Bridge) gatewayChanged(prev string, s gateway.State) {
	switch s.Name {
	case gateway.StateFatal:
		b.noticeDown("🔴 The Discord gateway stopped: " + s.Reason + ". Fix the cause and save the plugin settings to restart it.")
	case gateway.StateRefused:
		b.noticeDown("🔴 The Discord gateway is paused: " + s.Reason + ". It resumes automatically.")
	case gateway.StateReconnecting:
		if prev == gateway.StateConnected {
			b.timers.after("gateway-down", b.downNoticeDelay, func() {
				b.noticeDown("🔴 Disconnected from the Discord gateway; reconnecting. Messages posted meanwhile are delivered when it returns.")
			})
		}
	}
}

// noticeDown posts text to every guild whose last notice was "up", unless
// the gateway is connected again.
func (b *Bridge) noticeDown(text string) {
	b.mu.Lock()
	if b.health.GatewayState == gateway.StateConnected {
		b.mu.Unlock()
		return
	}
	var targets []store.Guild
	for _, g := range b.guilds {
		if b.notices[g.GuildID] == noticeUp {
			b.notices[g.GuildID] = noticeDown
			targets = append(targets, g)
		}
	}
	b.mu.Unlock()
	for _, g := range targets {
		b.sched.Do("guild:"+g.GuildID, func(ctx context.Context) { b.postStatus(ctx, g, text) })
	}
}
