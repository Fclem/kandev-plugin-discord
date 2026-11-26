package bridge

import (
	"context"
	"fmt"
	"log"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/gateway"
	"kandev-plugin-discord/server/store"
)

// guildNotices tracks the lifecycle notices posted to one guild's
// #kandev-status. Gateway outages and provisioning failures are tracked
// apart: each is cleared only by its own recovery, so a REST provisioning
// pass during a gateway outage never posts an all-clear. Routine gateway
// reconnects post nothing.
type guildNotices struct {
	announced bool // "connected" was posted
	outage    bool // an outage was posted and the gateway has not returned
	failed    bool // a provisioning failure was posted and not yet recovered
}

// noticesOf returns a guild's notice state; the caller holds b.mu.
func (b *Bridge) noticesOf(guildID string) *guildNotices {
	n := b.notices[guildID]
	if n == nil {
		n = &guildNotices{}
		b.notices[guildID] = n
	}
	return n
}

// noticeUp records a successful provisioning: it ends a provisioning
// failure, and, once the gateway is connected, an outage or the first start.
func (b *Bridge) noticeUp(ctx context.Context, binding config.Binding, g store.Guild) {
	b.mu.Lock()
	n := b.noticesOf(g.GuildID)
	connected := b.health.GatewayState == gateway.StateConnected
	var texts []string
	if n.failed {
		n.failed = false
		texts = append(texts, "🟢 Provisioning succeeded.")
	}
	if connected && n.outage {
		n.outage = false
		texts = append(texts, "🟢 Reconnected to Discord; Kandev updates resume.")
	}
	if connected && !n.announced {
		n.announced = true
		texts = append(texts, fmt.Sprintf("🟢 Connected: this server mirrors Kandev workspace `%s`.", binding.WorkspaceID))
	}
	b.mu.Unlock()
	for _, text := range texts {
		log.Printf("discord: server %s: %s", binding.GuildID, text)
		b.postStatus(ctx, g, text)
	}
}

// noticeProvisioningFailed announces the first failure of a streak, in the
// #kandev-status of the last good record (if any).
func (b *Bridge) noticeProvisioningFailed(ctx context.Context, binding config.Binding, err error) {
	if b.botID() == "" {
		return // READY has not arrived; provisioning simply waits for it
	}
	b.mu.Lock()
	n := b.noticesOf(binding.GuildID)
	first := !n.failed
	n.failed = true
	b.mu.Unlock()
	if !first {
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

// noticeDown posts text to every announced guild without a pending outage
// notice, unless the gateway is connected again.
func (b *Bridge) noticeDown(text string) {
	b.mu.Lock()
	if b.health.GatewayState == gateway.StateConnected {
		b.mu.Unlock()
		return
	}
	var targets []store.Guild
	for _, g := range b.guilds {
		if n := b.noticesOf(g.GuildID); n.announced && !n.outage {
			n.outage = true
			targets = append(targets, g)
		}
	}
	b.mu.Unlock()
	for _, g := range targets {
		b.sched.Do("guild:"+g.GuildID, func(ctx context.Context) { b.postStatus(ctx, g, text) })
	}
}
