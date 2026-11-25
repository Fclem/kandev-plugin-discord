// Package bridge wires Kandev events, the Discord gateway, and the feature
// packages together. Every piece of work for one task runs on that task's
// scheduler queue.
package bridge

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/commands"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/gateway"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/provision"
	"kandev-plugin-discord/server/store"
)

// Deps are the bridge's collaborators.
type Deps struct {
	Config config.Config
	API    discordapi.API
	Host   kandev.Host
	Store  *store.Store
	// Halted reports a REST 401; nil means never.
	Halted func() bool
	Now    func() time.Time
}

// Bridge is the running plugin.
type Bridge struct {
	cfg    config.Config
	api    discordapi.API
	host   kandev.Host
	st     *store.Store
	halted func() bool
	now    func() time.Time
	// started is the process start, after which `failed` channels retry.
	started time.Time

	ctx    context.Context
	sched  *Scheduler
	timers timers
	events *lru

	mu           sync.Mutex
	botUserID    string
	guilds       map[string]store.Guild // workspace → provisioned record
	health       store.Health
	syncing      map[string]bool      // task → sync queued
	queueCounts  map[string]int       // session → queued messages
	sessionTasks map[string]string    // session → task
	steps        map[string]stepCache // workflow → step names
}

// New returns an idle bridge; Run starts it.
func New(d Deps) *Bridge {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Halted == nil {
		d.Halted = func() bool { return false }
	}
	b := &Bridge{
		cfg: d.Config, api: d.API, host: d.Host, st: d.Store, halted: d.Halted, now: d.Now, started: d.Now(),
		events:  newLRU(4096),
		guilds:  map[string]store.Guild{},
		syncing: map[string]bool{},
	}
	b.health = store.Health{GatewayState: gateway.StateStopped}
	for _, binding := range d.Config.Bindings {
		b.health.Bindings = append(b.health.Bindings, store.BindingHealth{WorkspaceID: binding.WorkspaceID, GuildID: binding.GuildID})
	}
	b.bind(context.Background())
	return b
}

// bind sets the context jobs run with.
func (b *Bridge) bind(ctx context.Context) {
	b.ctx = ctx
	b.sched = NewScheduler(ctx)
}

// Run connects the gateway through sess and blocks until ctx ends or the
// gateway stops for good.
func (b *Bridge) Run(ctx context.Context, sess *discordgo.Session) gateway.State {
	b.bind(ctx)
	sup := &gateway.Supervisor{
		Conn:   sess,
		Budget: &gateway.StoreBudget{St: b.st},
		Halted: b.halted,
		Report: func(s gateway.State) { b.setGateway(ctx, s) },
	}
	sess.ShouldReconnectOnError = false
	sess.StateEnabled = false
	sess.Identify.Intents = gateway.Intents
	sess.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		sup.Identified(ctx)
		b.onReady(ctx, r)
	})
	sess.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) { b.onGuildCreate(ctx, g.Guild) })
	sess.AddHandler(func(_ *discordgo.Session, c *discordgo.ChannelDelete) { b.onChannelDelete(ctx, c.Channel) })
	sess.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { sup.Disconnected() })
	go b.reconcileLoop(ctx)
	final := sup.Run(ctx)
	b.timers.stopAll()
	return final
}

// onReady records the bot identity and reports bound guilds the bot is not
// a member of. Provisioning follows each GUILD_CREATE.
func (b *Bridge) onReady(ctx context.Context, r *discordgo.Ready) {
	member := map[string]bool{}
	for _, g := range r.Guilds {
		member[g.ID] = true
	}
	b.mu.Lock()
	if r.User != nil {
		b.botUserID = r.User.ID
	}
	for i := range b.health.Bindings {
		bh := &b.health.Bindings[i]
		bh.BotMember = member[bh.GuildID]
		bh.InviteURL = ""
		if !bh.BotMember {
			bh.InviteURL = discordapi.InviteURL(b.cfg.ApplicationID, bh.GuildID)
			log.Printf("discord: bot is not a member of server %s bound to workspace %s; invite it: %s", bh.GuildID, bh.WorkspaceID, bh.InviteURL)
		}
	}
	b.mu.Unlock()
	b.saveHealth(ctx)
}

// onGuildCreate provisions a bound guild, then reconciles its workspace:
// Discord does not replay events missed while the bridge was offline.
func (b *Bridge) onGuildCreate(ctx context.Context, g *discordgo.Guild) {
	binding, ok := b.cfg.BindingForGuild(g.ID)
	if !ok || g.Unavailable {
		return
	}
	b.sched.Do("guild:"+g.ID, func(ctx context.Context) {
		if b.provisionGuild(ctx, binding) {
			b.reconcileWorkspace(ctx, binding)
		}
	})
}

func (b *Bridge) provisionGuild(ctx context.Context, binding config.Binding) bool {
	rec, err := provision.Provision(ctx, b.api, b.st, provision.Input{
		Binding: binding, Config: b.cfg, BotUserID: b.botID(),
		Commands: commands.Definitions(), CommandsHash: commands.DefinitionsHash(b.cfg.ApplicationID), Now: b.now(),
	})
	b.mu.Lock()
	for i := range b.health.Bindings {
		bh := &b.health.Bindings[i]
		if bh.WorkspaceID != binding.WorkspaceID {
			continue
		}
		bh.BotMember, bh.Provisioned, bh.Error = true, err == nil, ""
		if err != nil {
			bh.Error = "provisioning failed: " + err.Error()
		}
	}
	if err == nil {
		b.guilds[binding.WorkspaceID] = rec
	}
	b.mu.Unlock()
	b.saveHealth(ctx)
	if err != nil {
		log.Printf("discord: provisioning server %s for workspace %s failed: %v", binding.GuildID, binding.WorkspaceID, err)
		return false
	}
	log.Printf("discord: server %s provisioned for workspace %s", binding.GuildID, binding.WorkspaceID)
	return true
}

func (b *Bridge) botID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.botUserID
}

// guild returns the provisioned record of a workspace.
func (b *Bridge) guild(workspaceID string) (store.Guild, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g, ok := b.guilds[workspaceID]
	return g, ok
}

// guildByID returns the provisioned record and binding of a guild.
func (b *Bridge) guildByID(guildID string) (store.Guild, config.Binding, bool) {
	binding, ok := b.cfg.BindingForGuild(guildID)
	if !ok {
		return store.Guild{}, binding, false
	}
	g, ok := b.guild(binding.WorkspaceID)
	return g, binding, ok
}

// setGuild replaces a provisioned record (category rollover).
func (b *Bridge) setGuild(ctx context.Context, workspaceID string, g store.Guild) error {
	b.mu.Lock()
	b.guilds[workspaceID] = g
	b.mu.Unlock()
	return b.st.PutGuild(ctx, workspaceID, g)
}

func (b *Bridge) setGateway(ctx context.Context, s gateway.State) {
	b.mu.Lock()
	b.health.GatewayState = s.Name
	b.health.Reason = s.Reason
	if s.CloseCode != 0 {
		b.health.LastCloseCode = s.CloseCode
	}
	if s.Identifies24h != 0 {
		b.health.IdentifyCount24h = s.Identifies24h
	}
	b.mu.Unlock()
	if s.Name == gateway.StateFatal || s.Name == gateway.StateHalted || s.Name == gateway.StateRefused {
		log.Printf("discord: gateway %s: %s", s.Name, s.Reason)
	}
	b.saveHealth(ctx)
}

func (b *Bridge) saveHealth(ctx context.Context) {
	b.mu.Lock()
	b.health.CheckedAt = b.now().UTC()
	h := b.health
	h.Bindings = append([]store.BindingHealth(nil), b.health.Bindings...)
	b.mu.Unlock()
	if err := b.st.PutHealth(context.WithoutCancel(ctx), h); err != nil {
		log.Printf("discord: saving health: %v", err)
	}
}

// postStatus writes one line to a guild's #kandev-status channel.
func (b *Bridge) postStatus(ctx context.Context, g store.Guild, text string) {
	if g.StatusChannelID == "" {
		return
	}
	if _, err := b.api.MessageSend(ctx, g.StatusChannelID, &discordgo.MessageSend{Content: text}); err != nil {
		log.Printf("discord: posting to #%s in %s: %v", provision.StatusChannelName, g.GuildID, err)
	}
}
