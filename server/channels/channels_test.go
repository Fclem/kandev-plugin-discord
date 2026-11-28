package channels

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/commands"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/provision"
	"kandev-plugin-discord/server/store"
)

const (
	guildID = "223456789012345678"
	appID   = "123456789012345678"
	botID   = "900000000000000001"
)

func TestName(t *testing.T) {
	cases := []struct {
		task pluginsdk.Task
		want string
	}{
		{pluginsdk.Task{Identifier: "KAN-42", Title: "Fix login"}, "kan-42-fix-login"},
		{pluginsdk.Task{Identifier: "KAN-7", Title: "Café: naïve résumé!! 🚀 v2.0"}, "kan-7-cafe-naive-resume-v2-0"},
		{pluginsdk.Task{ID: "0f3e9a2b-1111-2222", Title: "Untitled"}, "task-0f3e9a2b-untitled"},
		{pluginsdk.Task{Identifier: "KAN-1", Title: "   "}, "kan-1"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, Name(&tc.task))
	}
	long := Name(&pluginsdk.Task{Identifier: "KAN-9", Title: strings.Repeat("word ", 60)})
	assert.LessOrEqual(t, len(long), MaxNameLength)
	assert.False(t, strings.HasSuffix(long, "-"))
}

func TestTopicCarriesParentAndLink(t *testing.T) {
	topic := Topic(&pluginsdk.Task{Identifier: "KAN-2", Title: "Sub"}, "KAN-1", "https://k/t/2")
	assert.Equal(t, "Sub · KAN-2 · subtask of KAN-1 · https://k/t/2", topic)
}

type fixture struct {
	api   *discordapi.Fake
	m     *Manager
	guild store.Guild
	now   time.Time
	notes []string
}

func newFixture(t *testing.T, mutate ...func(*config.Config)) *fixture {
	t.Helper()
	cfg := config.Config{
		ApplicationID: appID, OperatorRoleName: "Kandev Operator", KandevBaseURL: "https://kandev.example.com",
		CloseTrigger: config.CloseOnArchived, ClosedChannelAction: config.ClosedArchive, ArchiveRetentionDays: 30,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	f := &fixture{api: discordapi.NewFake(), now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	g, err := provision.Provision(context.Background(), f.api, store.New(store.NewMemoryHost()), provision.Input{
		Binding: config.Binding{WorkspaceID: "ws", GuildID: guildID}, Config: cfg, BotUserID: botID,
		Commands: commands.Definitions(), CommandsHash: "h",
	})
	require.NoError(t, err)
	f.guild = g
	f.m = &Manager{
		API: f.api, Config: cfg, BotUserID: botID, StartedAt: f.now.Add(-time.Hour),
		SaveGuild: func(_ context.Context, _ string, g store.Guild) error { f.guild = g; return nil },
		Note:      func(_ context.Context, _ store.Guild, text string) { f.notes = append(f.notes, text) },
	}
	f.api.ResetCalls()
	return f
}

func (f *fixture) sync(t *testing.T, task *pluginsdk.Task, rec store.Channel) Result {
	t.Helper()
	res, err := f.m.Sync(context.Background(), Input{Task: task, Rec: rec, Guild: f.guild, Now: f.now})
	require.NoError(t, err)
	return res
}

func task() *pluginsdk.Task {
	return &pluginsdk.Task{ID: "t1", WorkspaceID: "ws", Identifier: "KAN-42", Title: "Fix login", State: "IN_PROGRESS", CreatedAt: "2026-11-25T11:59:00Z"}
}

func TestCreateOnceWithTopicAndCursors(t *testing.T) {
	f := newFixture(t)
	res := f.sync(t, task(), store.Channel{})
	require.True(t, res.Changed)
	rec := res.Rec
	assert.Equal(t, store.StatusActive, rec.Status)
	ch := f.api.Channels[rec.ChannelID]
	require.NotNil(t, ch)
	assert.Equal(t, "kan-42-fix-login", ch.Name)
	assert.Equal(t, f.guild.TaskCategoryIDs[0], ch.ParentID)
	assert.Contains(t, ch.Topic, "https://kandev.example.com/t/t1")
	assert.Equal(t, rec.ChannelID, rec.InboundCursor)
	assert.Equal(t, "2026-11-25T11:59:00Z", rec.OutboundCursor.CreatedAt)

	f.api.ResetCalls()
	again := f.sync(t, task(), rec)
	assert.False(t, again.Changed)
	assert.Equal(t, 0, f.api.Mutations())
}

func TestOldTaskMirrorsOnlyRecentBacklog(t *testing.T) {
	f := newFixture(t)
	old := task()
	old.CreatedAt = "2026-01-01T00:00:00Z"
	res := f.sync(t, old, store.Channel{})
	assert.Equal(t, f.now.Add(-InitialBacklog).Format(time.RFC3339Nano), res.Rec.OutboundCursor.CreatedAt)
}

func TestCapacityBlockedNotesOnceThenRecovers(t *testing.T) {
	f := newFixture(t)
	full := &discordapi.Error{Status: 400, Code: discordapi.CodeMaxGuildChannels}
	f.api.FailNext("GuildChannelCreate", full)
	f.api.FailNext("GuildChannelCreate", full)
	res := f.sync(t, task(), store.Channel{})
	assert.Equal(t, store.StatusCapacityBlocked, res.Rec.Status)
	res = f.sync(t, task(), res.Rec)
	assert.Equal(t, store.StatusCapacityBlocked, res.Rec.Status)
	assert.Len(t, f.notes, 1)
	res = f.sync(t, task(), res.Rec)
	assert.Equal(t, store.StatusActive, res.Rec.Status)
}

func TestForbiddenMarksFailedUntilRestart(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("GuildChannelCreate", &discordapi.Error{Status: 403, Code: discordapi.CodeMissingPermissions})
	res := f.sync(t, task(), store.Channel{})
	assert.Equal(t, store.StatusFailed, res.Rec.Status)
	f.api.ResetCalls()
	res = f.sync(t, task(), res.Rec)
	assert.Equal(t, store.StatusFailed, res.Rec.Status)
	assert.Equal(t, 0, f.api.Mutations(), "no retry in the same process")
	f.m.StartedAt = f.now.Add(time.Minute)
	res = f.sync(t, task(), res.Rec)
	assert.Equal(t, store.StatusActive, res.Rec.Status)
}

func TestCategoryRolloverAtFifty(t *testing.T) {
	f := newFixture(t)
	for range CategoryCapacity {
		f.api.AddChannel(&discordgo.Channel{GuildID: guildID, ParentID: f.guild.TaskCategoryIDs[0]})
	}
	res := f.sync(t, task(), store.Channel{})
	require.Len(t, f.guild.TaskCategoryIDs, 2)
	assert.Equal(t, "Kandev Tasks 2", f.api.Channels[f.guild.TaskCategoryIDs[1]].Name)
	assert.Equal(t, f.guild.TaskCategoryIDs[1], f.api.Channels[res.Rec.ChannelID].ParentID)
}

func TestRenameIsDebouncedAndLastTitleWins(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec

	renamed := task()
	renamed.Title = "Fix login on Safari"
	rec = f.sync(t, renamed, rec).Rec
	assert.Equal(t, "kan-42-fix-login-on-safari", f.api.Channels[rec.ChannelID].Name)

	renamed.Title = "Fix login everywhere"
	f.now = f.now.Add(3 * time.Minute)
	res := f.sync(t, renamed, rec)
	assert.False(t, res.Changed)
	assert.Equal(t, 7*time.Minute, res.RetryAfter)

	renamed.Title = "Fix login for good"
	f.now = f.now.Add(7 * time.Minute)
	rec = f.sync(t, renamed, res.Rec).Rec
	assert.Equal(t, "kan-42-fix-login-for-good", f.api.Channels[rec.ChannelID].Name)
}

func TestArchiveReopenAndRetention(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec

	archived := task()
	at := "2026-11-25T12:00:00Z"
	archived.ArchivedAt = &at
	rec = f.sync(t, archived, rec).Rec
	assert.Equal(t, store.StatusArchived, rec.Status)
	ch := f.api.Channels[rec.ChannelID]
	assert.Equal(t, f.guild.ArchiveCategoryIDs[0], ch.ParentID)
	assert.True(t, provision.SameOverwrites(ch.PermissionOverwrites, provision.ReadOnlyOverwrites(provision.RolesOf(f.guild, botID))))
	assert.Equal(t, f.now.Add(30*24*time.Hour), rec.DeleteAfter)

	rec = f.sync(t, task(), rec).Rec
	assert.Equal(t, store.StatusActive, rec.Status)
	assert.Equal(t, f.guild.TaskCategoryIDs[0], f.api.Channels[rec.ChannelID].ParentID)
	assert.True(t, rec.DeleteAfter.IsZero())

	rec = f.sync(t, archived, rec).Rec
	f.now = f.now.Add(31 * 24 * time.Hour)
	rec = f.sync(t, archived, rec).Rec
	assert.Equal(t, store.StatusDeleted, rec.Status)
	assert.Nil(t, f.api.Channels[rec.ChannelID])
}

func TestDeleteActionAndTerminalTrigger(t *testing.T) {
	f := newFixture(t, func(c *config.Config) {
		c.ClosedChannelAction = config.ClosedDelete
		c.CloseTrigger = config.CloseOnTerminal
	})
	rec := f.sync(t, task(), store.Channel{}).Rec
	done := task()
	done.State = "COMPLETED"
	rec = f.sync(t, done, rec).Rec
	assert.Equal(t, store.StatusDeleted, rec.Status)
	assert.Nil(t, f.api.Channels[rec.ChannelID])

	rec = f.sync(t, nil, rec).Rec
	assert.Equal(t, store.StatusDeleted, rec.Status, "deleted task stays deleted")
}

func TestManuallyDeletedChannelIsRecreatedWithNote(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	delete(f.api.Channels, rec.ChannelID)
	rec = MarkDeleted(rec, f.now)
	res := f.sync(t, task(), rec)
	assert.Equal(t, store.StatusActive, res.Rec.Status)
	assert.NotEqual(t, rec.ChannelID, res.Rec.ChannelID)
	require.Len(t, f.notes, 1)
	assert.Contains(t, f.notes[0], "Recreated")
}

func archivedTask() *pluginsdk.Task {
	t := task()
	at := "2026-11-25T12:00:00Z"
	t.ArchivedAt = &at
	return t
}

// Saving the settings restarts the plugin with the new retention, which
// applies to channels archived before: 0 keeps them, a lower value frees
// room sooner (the capacity note's advice).
func TestRetentionChangesApplyToArchivedChannels(t *testing.T) {
	f := newFixture(t) // retention 30
	rec := f.sync(t, task(), store.Channel{}).Rec
	rec = f.sync(t, archivedTask(), rec).Rec
	require.Equal(t, store.StatusArchived, rec.Status)

	f.m.Config.ArchiveRetentionDays = 0
	f.now = f.now.Add(31 * 24 * time.Hour)
	res := f.sync(t, archivedTask(), rec)
	assert.Equal(t, store.StatusArchived, res.Rec.Status, "retention 0 keeps the archived channel")
	assert.True(t, res.Changed, "the cleared deadline is saved")
	assert.NotNil(t, f.api.Channels[rec.ChannelID])

	f.m.Config.ArchiveRetentionDays = 1
	rec = f.sync(t, archivedTask(), res.Rec).Rec
	assert.Equal(t, store.StatusDeleted, rec.Status, "a 1-day retention deletes a 31-day-old archive")
	assert.Nil(t, f.api.Channels[rec.ChannelID])
}

// A channel the plugin deleted by policy is recreated without a note that
// claims someone deleted it in Discord.
func TestPolicyDeleteThenReopenIsNotReportedAsDeletedInDiscord(t *testing.T) {
	f := newFixture(t, func(c *config.Config) {
		c.ClosedChannelAction = config.ClosedDelete
		c.CloseTrigger = config.CloseOnTerminal
	})
	rec := f.sync(t, task(), store.Channel{}).Rec
	done := task()
	done.State = "COMPLETED"
	rec = f.sync(t, done, rec).Rec
	require.Equal(t, store.StatusDeleted, rec.Status)
	rec = f.sync(t, task(), rec).Rec
	require.Equal(t, store.StatusActive, rec.Status)
	for _, n := range f.notes {
		assert.NotContains(t, n, "deleted in Discord")
	}
}
