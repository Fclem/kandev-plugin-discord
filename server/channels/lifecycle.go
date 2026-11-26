package channels

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/provision"
	"kandev-plugin-discord/server/store"
)

// Limits and timings.
const (
	CategoryCapacity = 50
	RenameInterval   = 10 * time.Minute
	// InitialBacklog bounds how much existing conversation a new channel
	// mirrors: messages since the task's creation, but at most this old.
	InitialBacklog = 10 * time.Minute
)

// Manager applies the channel state machine for one task at a time.
type Manager struct {
	API       discordapi.API
	Config    config.Config
	BotUserID string
	// StartedAt is the process start: `failed` records are retried once per
	// process (after a restart or a config change, which restarts).
	StartedAt time.Time
	// CategoryLock serializes category selection, rollover, and the channel
	// placement that counts against it, across every task of the process.
	CategoryLock *sync.Mutex
	// CurrentGuild returns the latest guild record (after other tasks'
	// rollovers); nil means use Input.Guild.
	CurrentGuild func(workspaceID string) (store.Guild, bool)
	// SaveGuild persists a guild record after category rollover.
	SaveGuild func(ctx context.Context, workspaceID string, g store.Guild) error
	// SaveIntent persists the record before a channel is created, so a lost
	// write after creation is recovered by adopting the channel on retry.
	SaveIntent func(ctx context.Context, rec store.Channel) error
	// Owner returns the task a channel is routed to, if any; adoption never
	// takes another task's channel.
	Owner func(ctx context.Context, channelID string) (taskID string, found bool, err error)
	// Note posts one line to the guild's #kandev-status.
	Note func(ctx context.Context, g store.Guild, text string)
}

// Input is one task's observed state.
type Input struct {
	// Task is nil when Kandev no longer has the task.
	Task             *pluginsdk.Task
	Rec              store.Channel
	Guild            store.Guild
	ParentIdentifier string
	Now              time.Time
}

// Result is the new record and when to look again.
type Result struct {
	Rec        store.Channel
	Changed    bool
	RetryAfter time.Duration
}

// WantOpen reports whether a task should have an open channel.
func WantOpen(cfg config.Config, task *pluginsdk.Task) bool {
	if task == nil || task.ArchivedAt != nil || (task.IsEphemeral && !cfg.IncludeEphemeralTasks) {
		return false
	}
	if cfg.CloseTrigger == config.CloseOnTerminal && (task.State == "COMPLETED" || task.State == "CANCELLED") {
		return false
	}
	return true
}

// Sync moves the record one step toward the desired state.
func (m *Manager) Sync(ctx context.Context, in Input) (Result, error) {
	rec := in.Rec
	want := WantOpen(m.Config, in.Task)
	switch rec.Status {
	case "", store.StatusDeleted, store.StatusCapacityBlocked, store.StatusCreating:
		if !want {
			if rec.Status == store.StatusCreating && rec.Creation != nil {
				return m.abandon(ctx, in)
			}
			return Result{Rec: rec}, nil
		}
		return m.create(ctx, in)
	case store.StatusFailed:
		if !want || rec.UpdatedAt.After(m.StartedAt) {
			return Result{Rec: rec}, nil
		}
		return m.create(ctx, in)
	case store.StatusActive:
		if !want {
			return m.close(ctx, in)
		}
		res, err := m.Converge(ctx, in, false)
		if err != nil || res.Rec.Status != store.StatusActive {
			return res, err
		}
		next, err := m.rename(ctx, Input{Task: in.Task, Rec: res.Rec, Guild: in.Guild, ParentIdentifier: in.ParentIdentifier, Now: in.Now})
		next.Changed = next.Changed || res.Changed
		return next, err
	case store.StatusArchived:
		if want {
			return m.reopen(ctx, in)
		}
		if !rec.DeleteAfter.IsZero() && !in.Now.Before(rec.DeleteAfter) {
			return m.remove(ctx, in, "retention period ended")
		}
		res, err := m.Converge(ctx, in, true)
		if err != nil || res.Rec.Status != store.StatusArchived {
			return res, err
		}
		if !rec.DeleteAfter.IsZero() {
			res.RetryAfter = rec.DeleteAfter.Sub(in.Now)
		}
		return res, nil
	}
	return Result{Rec: rec}, nil
}

// abandon settles a creation whose task closed (or vanished) before its
// record was written: a channel Discord made for it is adopted, and the
// adoption is saved before anything else, so the next sync closes it like
// any other channel (archived or deleted) and a failure while closing is
// recovered from the record. No unrouted channel stays writable.
func (m *Manager) abandon(ctx context.Context, in Input) (Result, error) {
	rec := in.Rec
	var ch *discordgo.Channel
	err := m.place(ctx, in, func(p *placement) error {
		var err error
		ch, err = p.adoptable(ctx, *rec.Creation, rec.TaskID)
		return err
	})
	if err != nil {
		return Result{Rec: rec}, fmt.Errorf("looking for an unrecorded channel: %w", err)
	}
	made := *rec.Creation
	rec.Creation, rec.UpdatedAt, rec.LastError = nil, in.Now, ""
	if ch == nil {
		rec.Status = store.StatusDeleted // nothing was created
		return Result{Rec: rec, Changed: true}, nil
	}
	rec.ChannelID, rec.Status, rec.RolesKey = ch.ID, store.StatusActive, made.RolesKey
	rec.SyncedTitle, rec.SyncedTopic = made.Title, made.Topic
	if rec.OutboundCursor.CreatedAt == "" {
		// Nothing was ever mirrored there; the close posts nothing old.
		rec.OutboundCursor = store.Cursor{CreatedAt: in.Now.UTC().Format(time.RFC3339Nano)}
	}
	return Result{Rec: rec, Changed: true, RetryAfter: time.Second}, nil
}

// MarkDeleted records that Discord deleted the channel.
func MarkDeleted(rec store.Channel, now time.Time) store.Channel {
	rec.Status = store.StatusDeleted
	rec.LastError = "channel was deleted in Discord"
	rec.UpdatedAt = now
	return rec
}

func (m *Manager) roles(g store.Guild) provision.Roles { return provision.RolesOf(g, m.BotUserID) }

// RolesKey identifies the role set a channel's overwrites were written for.
func RolesKey(g store.Guild) string { return g.OperatorRoleID + "/" + g.ViewerRoleID }

func (m *Manager) topic(in Input) string {
	return Topic(in.Task, in.ParentIdentifier, m.Config.TaskURL(in.Task.ID))
}

// Converge rewrites a channel's overwrites after the operator or viewer
// role changed, so a former operator role loses access. Nothing may be
// posted to an active channel until its RolesKey matches the guild's.
func (m *Manager) Converge(ctx context.Context, in Input, readOnly bool) (Result, error) {
	rec := in.Rec
	key := RolesKey(in.Guild)
	if rec.RolesKey == key {
		return Result{Rec: rec}, nil
	}
	overwrites := provision.TaskOverwrites(m.roles(in.Guild))
	if readOnly {
		overwrites = provision.ReadOnlyOverwrites(m.roles(in.Guild))
	}
	if _, err := m.API.ChannelEdit(ctx, rec.ChannelID, &discordgo.ChannelEdit{PermissionOverwrites: overwrites}); err != nil {
		if discordapi.IsGone(err) {
			return Result{Rec: MarkDeleted(rec, in.Now), Changed: true}, nil
		}
		return Result{Rec: rec}, fmt.Errorf("updating channel permissions: %w", err)
	}
	rec.RolesKey, rec.UpdatedAt = key, in.Now
	return Result{Rec: rec, Changed: true}, nil
}

// placement holds the category lock while a channel is created or moved.
type placement struct {
	m        *Manager
	guild    store.Guild
	channels []*discordgo.Channel
}

// place locks category selection, refreshes the guild record, lists the
// guild's channels, and runs fn. fn's channel creation or move happens
// under the lock, so concurrent tasks see each other's placements.
func (m *Manager) place(ctx context.Context, in Input, fn func(p *placement) error) error {
	if m.CategoryLock != nil {
		m.CategoryLock.Lock()
		defer m.CategoryLock.Unlock()
	}
	g := in.Guild
	if m.CurrentGuild != nil {
		if cur, ok := m.CurrentGuild(workspaceOf(in)); ok && cur.GuildID == g.GuildID {
			g = cur
		}
	}
	channels, err := m.API.GuildChannels(ctx, g.GuildID)
	if err != nil {
		return fmt.Errorf("listing channels: %w", err)
	}
	return fn(&placement{m: m, guild: g, channels: channels})
}

func workspaceOf(in Input) string {
	if in.Task != nil {
		return in.Task.WorkspaceID
	}
	return in.Rec.WorkspaceID
}

// create makes the task's channel. Before asking Discord it saves the
// record as `creating` with what it asks for (name, topic, time), so a
// retry after a lost record write adopts the channel Discord made, even if
// the task was renamed meanwhile, instead of creating a duplicate.
func (m *Manager) create(ctx context.Context, in Input) (Result, error) {
	prev := in.Rec
	rec := prev
	rec.TaskID, rec.WorkspaceID, rec.GuildID = in.Task.ID, in.Task.WorkspaceID, in.Guild.GuildID
	rec.UpdatedAt = in.Now
	want := store.Creation{Name: Name(in.Task), Topic: m.topic(in), Title: in.Task.Title, At: in.Now}
	var (
		ch   *discordgo.Channel
		made store.Creation
	)
	err := m.place(ctx, in, func(p *placement) error {
		if prev.Status == store.StatusCreating && prev.Creation != nil {
			found, err := p.adoptable(ctx, *prev.Creation, in.Task.ID)
			if err != nil || found != nil {
				ch, made = found, *prev.Creation
				return err
			}
		}
		parent, err := p.categoryWithRoom(ctx, workspaceOf(in), false)
		if err != nil {
			return err
		}
		want.RolesKey = RolesKey(p.guild)
		if m.SaveIntent != nil {
			intent := rec
			// No channel yet: a former (deleted) channel must not be routed.
			intent.ChannelID, intent.Status, intent.Creation = "", store.StatusCreating, &want
			if err := m.SaveIntent(ctx, intent); err != nil {
				return fmt.Errorf("saving creation intent: %w", err)
			}
		}
		ch, err = m.API.GuildChannelCreate(ctx, p.guild.GuildID, discordgo.GuildChannelCreateData{
			Name: want.Name, Type: discordgo.ChannelTypeGuildText, Topic: want.Topic,
			ParentID: parent, PermissionOverwrites: provision.TaskOverwrites(m.roles(p.guild)),
		})
		made = want
		return err
	})
	if err == nil {
		rec.ChannelID, rec.Status, rec.LastError, rec.Creation = ch.ID, store.StatusActive, "", nil
		// An adopted channel carries the name, topic, and overwrites of the
		// original request: rename converges the name, and a RolesKey that
		// is not the guild's makes Converge rewrite the overwrites before
		// anything is posted.
		rec.SyncedTitle, rec.SyncedTopic, rec.LastRenamedAt = made.Title, made.Topic, time.Time{}
		rec.RolesKey = made.RolesKey
		rec.InboundCursor, rec.InboundInflight = ch.ID, ""
		rec.CardMessageID, rec.CardHash, rec.CardPinned = "", "", false
		rec.ClosedAt, rec.DeleteAfter = time.Time{}, time.Time{}
		if prev.OutboundCursor.CreatedAt == "" {
			rec.OutboundCursor = store.Cursor{CreatedAt: initialCursor(in.Task, in.Now)}
		}
		if prev.Status == store.StatusDeleted && prev.ChannelID != "" && m.Note != nil {
			m.Note(ctx, in.Guild, fmt.Sprintf("Recreated the channel of %s (#%s) after it was deleted in Discord.", label(in.Task), ch.Name))
		}
		res := Result{Rec: rec, Changed: true}
		if made.Title != in.Task.Title || made.RolesKey != RolesKey(in.Guild) {
			res.RetryAfter = time.Second // converge the adopted channel promptly
		}
		return res, nil
	}
	rec.Creation = nil // the outcomes below are definite: nothing was created
	switch {
	case discordapi.HasCode(err, discordapi.CodeMaxGuildChannels):
		rec.Status, rec.LastError = store.StatusCapacityBlocked, "the server reached Discord's 500-channel limit"
		if prev.Status != store.StatusCapacityBlocked && m.Note != nil {
			m.Note(ctx, in.Guild, fmt.Sprintf("Cannot create a channel for %s: the server has 500 channels. Delete archived channels or lower the archive retention; the plugin retries every 5 minutes.", label(in.Task)))
		}
		return Result{Rec: rec, Changed: true}, nil
	case discordapi.IsForbidden(err) || discordapi.Status(err) == 404:
		rec.Status, rec.LastError = store.StatusFailed, err.Error()
		return Result{Rec: rec, Changed: true}, nil
	}
	// Unknown outcome: the saved intent (if any) lets the retry adopt.
	return Result{Rec: prev}, fmt.Errorf("creating channel: %w", err)
}

// adoptSkew tolerates clock skew between this host and Discord's IDs.
const adoptSkew = time.Minute

// adoptable finds the channel an earlier attempt asked Discord for: a text
// channel in a plugin task category with the requested name and topic,
// created after the request, and not routed to another task.
func (p *placement) adoptable(ctx context.Context, c store.Creation, taskID string) (*discordgo.Channel, error) {
	for _, ch := range p.channels {
		if ch.Type != discordgo.ChannelTypeGuildText || ch.Name != c.Name || strings.TrimSpace(ch.Topic) != strings.TrimSpace(c.Topic) {
			continue
		}
		if !slices.Contains(p.guild.TaskCategoryIDs, ch.ParentID) {
			continue
		}
		if created, err := discordgo.SnowflakeTimestamp(ch.ID); err != nil || created.Before(c.At.Add(-adoptSkew)) {
			continue
		}
		if p.m.Owner != nil {
			owner, found, err := p.m.Owner(ctx, ch.ID)
			if err != nil {
				return nil, fmt.Errorf("checking channel ownership: %w", err)
			}
			if found && owner != taskID {
				continue
			}
		}
		return ch, nil
	}
	return nil, nil
}

func initialCursor(task *pluginsdk.Task, now time.Time) string {
	floor := now.Add(-InitialBacklog).UTC()
	created, err := time.Parse(time.RFC3339Nano, task.CreatedAt)
	if err != nil || created.Before(floor) {
		return floor.Format(time.RFC3339Nano)
	}
	return task.CreatedAt
}

func (m *Manager) rename(ctx context.Context, in Input) (Result, error) {
	rec := in.Rec
	topic := m.topic(in)
	if in.Task.Title == rec.SyncedTitle && topic == rec.SyncedTopic {
		return Result{Rec: rec}, nil
	}
	if wait := RenameInterval - in.Now.Sub(rec.LastRenamedAt); !rec.LastRenamedAt.IsZero() && wait > 0 {
		return Result{Rec: rec, RetryAfter: wait}, nil
	}
	edit := &discordgo.ChannelEdit{Topic: topic}
	if in.Task.Title != rec.SyncedTitle {
		edit.Name = Name(in.Task)
	}
	if _, err := m.API.ChannelEdit(ctx, rec.ChannelID, edit); err != nil {
		if discordapi.IsGone(err) {
			return Result{Rec: MarkDeleted(rec, in.Now), Changed: true}, nil
		}
		return Result{Rec: rec}, fmt.Errorf("renaming channel: %w", err)
	}
	rec.SyncedTitle, rec.SyncedTopic, rec.LastRenamedAt, rec.UpdatedAt = in.Task.Title, topic, in.Now, in.Now
	return Result{Rec: rec, Changed: true}, nil
}

func (m *Manager) close(ctx context.Context, in Input) (Result, error) {
	if m.Config.ClosedChannelAction == config.ClosedDelete {
		return m.remove(ctx, in, "task closed")
	}
	return m.move(ctx, in, true)
}

func (m *Manager) reopen(ctx context.Context, in Input) (Result, error) {
	return m.move(ctx, in, false)
}

// move archives (read-only, archive category) or reopens a channel.
func (m *Manager) move(ctx context.Context, in Input, archive bool) (Result, error) {
	rec := in.Rec
	err := m.place(ctx, in, func(p *placement) error {
		parent, err := p.categoryWithRoom(ctx, workspaceOf(in), archive)
		if err != nil {
			return err
		}
		overwrites := provision.TaskOverwrites(m.roles(p.guild))
		if archive {
			overwrites = provision.ReadOnlyOverwrites(m.roles(p.guild))
		}
		_, err = m.API.ChannelEdit(ctx, rec.ChannelID, &discordgo.ChannelEdit{ParentID: parent, PermissionOverwrites: overwrites})
		return err
	})
	if err != nil {
		if discordapi.IsGone(err) {
			return Result{Rec: MarkDeleted(rec, in.Now), Changed: true}, nil
		}
		verb := "reopening"
		if archive {
			verb = "archiving"
		}
		return Result{Rec: rec}, fmt.Errorf("%s channel: %w", verb, err)
	}
	rec.RolesKey, rec.UpdatedAt, rec.CloseRequestedAt = RolesKey(in.Guild), in.Now, time.Time{}
	if !archive {
		rec.Status, rec.ClosedAt, rec.DeleteAfter = store.StatusActive, time.Time{}, time.Time{}
		return Result{Rec: rec, Changed: true}, nil
	}
	rec.Status, rec.ClosedAt, rec.DeleteAfter = store.StatusArchived, in.Now, time.Time{}
	if days := m.Config.ArchiveRetentionDays; days > 0 {
		rec.DeleteAfter = in.Now.Add(time.Duration(days) * 24 * time.Hour)
	}
	return Result{Rec: rec, Changed: true}, nil
}

func (m *Manager) remove(ctx context.Context, in Input, why string) (Result, error) {
	rec := in.Rec
	if err := m.API.ChannelDelete(ctx, rec.ChannelID); err != nil && !discordapi.IsGone(err) {
		return Result{Rec: rec}, fmt.Errorf("deleting channel (%s): %w", why, err)
	}
	rec.Status, rec.UpdatedAt, rec.LastError = store.StatusDeleted, in.Now, ""
	return Result{Rec: rec, Changed: true}, nil
}

// categoryWithRoom returns a task (or archive) category with fewer than 50
// channels, creating the next one ("Kandev Tasks 2", …) when all are full.
// Categories deleted in Discord are dropped from the record first, so
// creation never targets a parent that no longer exists. The caller holds
// the category lock.
func (p *placement) categoryWithRoom(ctx context.Context, workspaceID string, archive bool) (string, error) {
	m, g := p.m, p.guild
	ids, base, overwrites := g.TaskCategoryIDs, provision.TaskCategoryName, provision.TaskOverwrites(m.roles(g))
	if archive {
		ids, base, overwrites = g.ArchiveCategoryIDs, provision.ArchiveCategoryName, provision.ReadOnlyOverwrites(m.roles(g))
	}
	counts := map[string]int{}
	isCategory := map[string]bool{}
	for _, c := range p.channels {
		counts[c.ParentID]++
		if c.Type == discordgo.ChannelTypeGuildCategory {
			isCategory[c.ID] = true
		}
	}
	kept := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return !isCategory[id] })
	for _, id := range kept {
		if counts[id] < CategoryCapacity {
			if len(kept) == len(ids) {
				return id, nil
			}
			return id, p.saveCategories(ctx, workspaceID, archive, kept)
		}
	}
	name := base
	if len(kept) > 0 {
		name = fmt.Sprintf("%s %d", base, len(kept)+1)
	}
	ch, err := provision.CreateCategory(ctx, m.API, g.GuildID, name, overwrites)
	if err != nil {
		return "", err
	}
	return ch.ID, p.saveCategories(ctx, workspaceID, archive, append(kept, ch.ID))
}

// saveCategories stores a new task (or archive) category list.
func (p *placement) saveCategories(ctx context.Context, workspaceID string, archive bool, ids []string) error {
	g := p.guild
	if archive {
		g.ArchiveCategoryIDs = ids
	} else {
		g.TaskCategoryIDs = ids
	}
	p.guild = g
	if p.m.SaveGuild == nil {
		return nil
	}
	return p.m.SaveGuild(ctx, workspaceID, g)
}

func label(t *pluginsdk.Task) string {
	if t.Identifier != "" {
		return t.Identifier + " " + fmt.Sprintf("%q", t.Title)
	}
	return fmt.Sprintf("%q", t.Title)
}
