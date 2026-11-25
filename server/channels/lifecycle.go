package channels

import (
	"context"
	"fmt"
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
	// SaveGuild persists a guild record after category rollover.
	SaveGuild func(ctx context.Context, workspaceID string, g store.Guild) error
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
		return m.rename(ctx, in)
	case store.StatusArchived:
		if want {
			return m.reopen(ctx, in)
		}
		if !rec.DeleteAfter.IsZero() && !in.Now.Before(rec.DeleteAfter) {
			return m.remove(ctx, in, "retention period ended")
		}
		if !rec.DeleteAfter.IsZero() {
			return Result{Rec: rec, RetryAfter: rec.DeleteAfter.Sub(in.Now)}, nil
		}
	}
	return Result{Rec: rec}, nil
}

// MarkDeleted records that Discord deleted the channel.
func MarkDeleted(rec store.Channel, now time.Time) store.Channel {
	rec.Status = store.StatusDeleted
	rec.LastError = "channel was deleted in Discord"
	rec.UpdatedAt = now
	return rec
}

func (m *Manager) roles(g store.Guild) provision.Roles { return provision.RolesOf(g, m.BotUserID) }

func (m *Manager) topic(in Input) string {
	return Topic(in.Task, in.ParentIdentifier, m.Config.TaskURL(in.Task.ID))
}

func (m *Manager) create(ctx context.Context, in Input) (Result, error) {
	prev := in.Rec
	rec := prev
	rec.TaskID, rec.WorkspaceID, rec.GuildID = in.Task.ID, in.Task.WorkspaceID, in.Guild.GuildID
	rec.UpdatedAt = in.Now
	parent, err := m.categoryWithRoom(ctx, in, false)
	if err == nil {
		var ch *discordgo.Channel
		ch, err = m.API.GuildChannelCreate(ctx, in.Guild.GuildID, discordgo.GuildChannelCreateData{
			Name: Name(in.Task), Type: discordgo.ChannelTypeGuildText, Topic: m.topic(in),
			ParentID: parent, PermissionOverwrites: provision.TaskOverwrites(m.roles(in.Guild)),
		})
		if err == nil {
			rec.ChannelID, rec.Status, rec.LastError = ch.ID, store.StatusActive, ""
			rec.SyncedTitle, rec.SyncedTopic, rec.LastRenamedAt = in.Task.Title, m.topic(in), time.Time{}
			rec.InboundCursor, rec.InboundInflight = ch.ID, ""
			rec.CardMessageID, rec.CardHash, rec.ClosedAt, rec.DeleteAfter = "", "", time.Time{}, time.Time{}
			if prev.OutboundCursor.CreatedAt == "" {
				rec.OutboundCursor = store.Cursor{CreatedAt: initialCursor(in.Task, in.Now)}
			}
			if prev.Status == store.StatusDeleted && prev.ChannelID != "" && m.Note != nil {
				m.Note(ctx, in.Guild, fmt.Sprintf("Recreated the channel of %s (#%s) after it was deleted in Discord.", label(in.Task), ch.Name))
			}
			return Result{Rec: rec, Changed: true}, nil
		}
	}
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
	return Result{Rec: prev}, fmt.Errorf("creating channel: %w", err)
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
	rec := in.Rec
	parent, err := m.categoryWithRoom(ctx, in, true)
	if err != nil {
		return Result{Rec: rec}, err
	}
	if _, err := m.API.ChannelEdit(ctx, rec.ChannelID, &discordgo.ChannelEdit{
		ParentID: parent, PermissionOverwrites: provision.ReadOnlyOverwrites(m.roles(in.Guild)),
	}); err != nil {
		if discordapi.IsGone(err) {
			return Result{Rec: MarkDeleted(rec, in.Now), Changed: true}, nil
		}
		return Result{Rec: rec}, fmt.Errorf("archiving channel: %w", err)
	}
	rec.Status, rec.ClosedAt, rec.UpdatedAt, rec.DeleteAfter = store.StatusArchived, in.Now, in.Now, time.Time{}
	if days := m.Config.ArchiveRetentionDays; days > 0 {
		rec.DeleteAfter = in.Now.Add(time.Duration(days) * 24 * time.Hour)
	}
	return Result{Rec: rec, Changed: true}, nil
}

func (m *Manager) reopen(ctx context.Context, in Input) (Result, error) {
	rec := in.Rec
	parent, err := m.categoryWithRoom(ctx, in, false)
	if err != nil {
		return Result{Rec: rec}, err
	}
	if _, err := m.API.ChannelEdit(ctx, rec.ChannelID, &discordgo.ChannelEdit{
		ParentID: parent, PermissionOverwrites: provision.TaskOverwrites(m.roles(in.Guild)),
	}); err != nil {
		if discordapi.IsGone(err) {
			return Result{Rec: MarkDeleted(rec, in.Now), Changed: true}, nil
		}
		return Result{Rec: rec}, fmt.Errorf("reopening channel: %w", err)
	}
	rec.Status, rec.ClosedAt, rec.DeleteAfter, rec.UpdatedAt = store.StatusActive, time.Time{}, time.Time{}, in.Now
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
func (m *Manager) categoryWithRoom(ctx context.Context, in Input, archive bool) (string, error) {
	g := in.Guild
	ids, base, overwrites := g.TaskCategoryIDs, provision.TaskCategoryName, provision.TaskOverwrites(m.roles(g))
	if archive {
		ids, base, overwrites = g.ArchiveCategoryIDs, provision.ArchiveCategoryName, provision.ReadOnlyOverwrites(m.roles(g))
	}
	channels, err := m.API.GuildChannels(ctx, g.GuildID)
	if err != nil {
		return "", fmt.Errorf("listing channels: %w", err)
	}
	counts := map[string]int{}
	for _, c := range channels {
		counts[c.ParentID]++
	}
	for _, id := range ids {
		if counts[id] < CategoryCapacity {
			return id, nil
		}
	}
	ch, err := provision.CreateCategory(ctx, m.API, g.GuildID, fmt.Sprintf("%s %d", base, len(ids)+1), overwrites)
	if err != nil {
		return "", err
	}
	if archive {
		g.ArchiveCategoryIDs = append(append([]string(nil), ids...), ch.ID)
	} else {
		g.TaskCategoryIDs = append(append([]string(nil), ids...), ch.ID)
	}
	if m.SaveGuild != nil {
		workspaceID := in.Rec.WorkspaceID
		if in.Task != nil {
			workspaceID = in.Task.WorkspaceID
		}
		if err := m.SaveGuild(ctx, workspaceID, g); err != nil {
			return "", err
		}
	}
	return ch.ID, nil
}

func label(t *pluginsdk.Task) string {
	if t.Identifier != "" {
		return t.Identifier + " " + fmt.Sprintf("%q", t.Title)
	}
	return fmt.Sprintf("%q", t.Title)
}
