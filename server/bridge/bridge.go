// Package bridge wires Kandev events, the Discord gateway, and the feature
// packages together. Every piece of work for one task runs on that task's
// scheduler queue.
package bridge

import (
	"context"
	"errors"
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
	// Context is what every job runs with; cancel it to stop the bridge.
	Context context.Context
	Config  config.Config
	API     discordapi.API
	Host    kandev.Host
	Store   *store.Store
	// Halted reports a REST 401; nil means never.
	Halted func() bool
	// HaltNotifier, when set, wakes the gateway on the first REST 401.
	HaltNotifier interface{ OnHalt(func()) }
	Now          func() time.Time
}

// Bridge is the running plugin.
type Bridge struct {
	cfg          config.Config
	api          discordapi.API
	host         kandev.Host
	st           *store.Store
	halted       func() bool
	haltNotifier interface{ OnHalt(func()) }
	now          func() time.Time
	// started is the process start, after which `failed` channels retry.
	started time.Time

	ctx    context.Context
	sched  *Scheduler
	timers timers
	events *lru
	// categoryMu serializes category selection and rollover (channels).
	categoryMu sync.Mutex

	mu           sync.Mutex
	botUserID    string
	guilds       map[string]store.Guild // workspace → provisioned record
	health       store.Health
	syncing      map[string]bool      // task → sync queued
	queueCounts  map[string]int       // session → queued messages
	sessionTasks map[string]string    // session → task
	steps        map[string]stepCache // workflow → step names
	// readyGen counts READY events; caughtUp records, per channel, the
	// generation whose missed messages were delivered.
	readyGen int
	caughtUp map[string]int
	// notices is the last lifecycle notice posted per guild (notices.go).
	notices         map[string]string
	downNoticeDelay time.Duration
}

// New returns an idle bridge; Run starts the gateway. The scheduler exists
// from construction, so events may be handled before Run.
func New(d Deps) *Bridge {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Halted == nil {
		d.Halted = func() bool { return false }
	}
	if d.Context == nil {
		d.Context = context.Background()
	}
	b := &Bridge{
		cfg: d.Config, api: d.API, host: d.Host, st: d.Store, halted: d.Halted, haltNotifier: d.HaltNotifier,
		now: d.Now, started: d.Now(), ctx: d.Context, sched: NewScheduler(d.Context),
		events:   newLRU(4096),
		guilds:   map[string]store.Guild{},
		syncing:  map[string]bool{},
		caughtUp: map[string]int{},
		notices:  map[string]string{},
		// A routine reconnect takes seconds; only a longer outage is noted.
		downNoticeDelay: time.Minute,
	}
	b.health = store.Health{GatewayState: gateway.StateStopped}
	for _, binding := range d.Config.Bindings {
		b.health.Bindings = append(b.health.Bindings, store.BindingHealth{WorkspaceID: binding.WorkspaceID, GuildID: binding.GuildID})
	}
	return b
}

// Run connects the gateway through sess and blocks until the bridge context
// ends or the gateway stops for good.
func (b *Bridge) Run(sess *discordgo.Session) gateway.State {
	ctx := b.ctx
	sup := &gateway.Supervisor{
		Conn:   sess,
		Budget: &gateway.StoreBudget{St: b.st},
		Halted: b.halted,
		Report: func(s gateway.State) { b.setGateway(ctx, s) },
	}
	if b.haltNotifier != nil {
		b.haltNotifier.OnHalt(sup.Halt)
	}
	gateway.Observe(sess, sup)
	sess.ShouldReconnectOnError = false
	sess.StateEnabled = false
	sess.Identify.Intents = gateway.Intents
	// Handlers run on the gateway goroutine in event order and only queue
	// work, so per-channel message order survives into the task queue.
	sess.SyncEvents = true
	sess.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) { b.onReady(ctx, r) })
	sess.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) { b.onGuildCreate(ctx, g.Guild) })
	sess.AddHandler(func(_ *discordgo.Session, c *discordgo.ChannelDelete) { b.onChannelDelete(ctx, c.Channel) })
	sess.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) { b.onMessageCreate(ctx, m) })
	sess.AddHandler(func(_ *discordgo.Session, i *discordgo.InteractionCreate) { go b.onInteraction(ctx, i) })
	sess.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { sup.Disconnected() })
	go b.reconcileLoop(ctx)
	final := sup.Run(ctx)
	b.timers.stopAll()
	return final
}

// onReady records the bot identity before any GUILD_CREATE is handled and
// reports bound guilds the bot is not a member of. Provisioning follows
// each GUILD_CREATE.
func (b *Bridge) onReady(ctx context.Context, r *discordgo.Ready) {
	member := map[string]bool{}
	for _, g := range r.Guilds {
		member[g.ID] = true
	}
	b.mu.Lock()
	if r.User != nil {
		b.botUserID = r.User.ID
	}
	b.readyGen++
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
	b.mu.Lock()
	for i := range b.health.Bindings {
		if b.health.Bindings[i].GuildID == g.ID {
			b.health.Bindings[i].BotMember, b.health.Bindings[i].InviteURL = true, ""
		}
	}
	b.mu.Unlock()
	b.sched.Do("guild:"+g.ID, func(ctx context.Context) { b.provisionAndReconcile(ctx, binding) })
}

// provisionAndReconcile provisions a guild and reconciles its workspace. A
// failed re-provisioning of a known guild still reconciles with the record
// it has.
func (b *Bridge) provisionAndReconcile(ctx context.Context, binding config.Binding) {
	_, known := b.guild(binding.WorkspaceID)
	if b.provisionGuild(ctx, binding) || known {
		b.reconcileWorkspace(ctx, binding)
	}
}

// provisionGuild needs the bot's user ID for its channel overwrites; a
// failed attempt is retried by the periodic reconcile. It holds the
// category lock: channel placement rewrites the same guild record (category
// rollover), and neither may overwrite the other's update.
func (b *Bridge) provisionGuild(ctx context.Context, binding config.Binding) bool {
	botID := b.botID()
	var (
		rec store.Guild
		err error
	)
	b.categoryMu.Lock()
	_, known := b.guild(binding.WorkspaceID)
	if botID == "" {
		err = errors.New("the bot identity is not known yet (no READY)")
	} else {
		rec, err = provision.Provision(ctx, b.api, b.st, provision.Input{
			Binding: binding, Config: b.cfg, BotUserID: botID,
			Commands: commands.Definitions(), CommandsHash: commands.DefinitionsHash(b.cfg.ApplicationID), Now: b.now(),
		})
	}
	if err != nil && known {
		// Provision stores a role change before its later steps: run on the
		// stored record, so channels converge to the new roles and the
		// former ones are trusted no longer.
		stored, found, serr := b.st.Guild(ctx, binding.WorkspaceID)
		b.mu.Lock()
		switch {
		case serr != nil:
			// Roles unknown: hold the workspace rather than trust old ones.
			delete(b.guilds, binding.WorkspaceID)
		case found && stored.GuildID == binding.GuildID:
			b.guilds[binding.WorkspaceID] = stored
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	for i := range b.health.Bindings {
		bh := &b.health.Bindings[i]
		if bh.WorkspaceID != binding.WorkspaceID {
			continue
		}
		bh.BotMember, bh.Provisioned, bh.Error = true, err == nil || known, ""
		if err != nil {
			bh.Error = "provisioning failed: " + err.Error()
		}
	}
	if err == nil {
		b.guilds[binding.WorkspaceID] = rec
	}
	b.mu.Unlock()
	b.categoryMu.Unlock()
	b.saveHealth(ctx)
	if err != nil {
		log.Printf("discord: provisioning server %s for workspace %s failed: %v", binding.GuildID, binding.WorkspaceID, err)
		b.noticeProvisioningFailed(ctx, binding, err)
		return false
	}
	b.noticeUp(ctx, binding, rec)
	return true
}

// isMember reports whether the bot is known to be in a binding's guild.
func (b *Bridge) isMember(workspaceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, bh := range b.health.Bindings {
		if bh.WorkspaceID == workspaceID {
			return bh.BotMember
		}
	}
	return false
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
	prev := b.health.GatewayState
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
	if s.Name != prev {
		b.gatewayChanged(prev, s)
	}
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
