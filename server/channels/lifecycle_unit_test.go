package channels

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/provision"
	"kandev-plugin-discord/server/store"
)

func (f *fixture) run(task *pluginsdk.Task, rec store.Channel) (Result, error) {
	return f.m.Sync(context.Background(), Input{Task: task, Rec: rec, Guild: f.guild, Now: f.now})
}

// creating returns the record a lost write after creation leaves behind,
// with the intent the manager saved.
func (f *fixture) creating(t *testing.T, task *pluginsdk.Task) store.Channel {
	t.Helper()
	var intent store.Channel
	f.m.SaveIntent = func(_ context.Context, rec store.Channel) error { intent = rec; return nil }
	_, err := f.run(task, store.Channel{})
	require.NoError(t, err)
	f.m.SaveIntent = nil
	require.Equal(t, store.StatusCreating, intent.Status)
	return intent
}

func TestWantOpen(t *testing.T) {
	cfg := config.Config{CloseTrigger: config.CloseOnArchived}
	eph := &pluginsdk.Task{IsEphemeral: true}
	assert.False(t, WantOpen(cfg, nil))
	assert.False(t, WantOpen(cfg, eph))
	assert.True(t, WantOpen(config.Config{IncludeEphemeralTasks: true}, eph))
	assert.True(t, WantOpen(cfg, &pluginsdk.Task{State: "COMPLETED"}), "archive trigger keeps completed tasks open")
	assert.False(t, WantOpen(config.Config{CloseTrigger: config.CloseOnTerminal}, &pluginsdk.Task{State: "CANCELLED"}))
}

func TestTopicWithoutIdentifierAndTruncation(t *testing.T) {
	assert.Equal(t, "Title", Topic(&pluginsdk.Task{Title: " Title "}, "", ""))
	long := Topic(&pluginsdk.Task{Title: strings.Repeat("é", 2000)}, "", "")
	assert.Len(t, []rune(long), maxTopicLength)
	assert.True(t, strings.HasSuffix(long, "…"))
}

func TestSaveIntentPrecedesCreation(t *testing.T) {
	f := newFixture(t)
	var intent store.Channel
	var channelsAtIntent int
	f.m.SaveIntent = func(_ context.Context, rec store.Channel) error {
		intent = rec
		channelsAtIntent = len(f.api.CallsTo("GuildChannelCreate"))
		return nil
	}
	prev := MarkDeleted(store.Channel{TaskID: "t1", ChannelID: "old"}, f.now)
	res, err := f.run(task(), prev)
	require.NoError(t, err)
	assert.Zero(t, channelsAtIntent, "the intent is saved before Discord is asked")
	assert.Equal(t, store.StatusCreating, intent.Status)
	assert.Empty(t, intent.ChannelID, "a deleted channel is not routed while creating")
	require.NotNil(t, intent.Creation)
	assert.Equal(t, "kan-42-fix-login", intent.Creation.Name)
	assert.Equal(t, RolesKey(f.guild), intent.Creation.RolesKey)
	assert.Nil(t, res.Rec.Creation)
	assert.Equal(t, store.StatusActive, res.Rec.Status)
}

func TestSaveIntentFailureCreatesNothing(t *testing.T) {
	f := newFixture(t)
	f.m.SaveIntent = func(context.Context, store.Channel) error { return errors.New("state down") }
	res, err := f.run(task(), store.Channel{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saving creation intent")
	assert.Empty(t, f.api.CallsTo("GuildChannelCreate"))
	assert.Empty(t, res.Rec.Status, "the previous record is kept")
}

func TestUnknownCreationOutcomeKeepsThePreviousRecord(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("GuildChannelCreate", &discordapi.Error{Status: 502})
	prev := store.Channel{TaskID: "t1", Status: store.StatusCapacityBlocked}
	res, err := f.run(task(), prev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating channel")
	assert.Equal(t, prev, res.Rec)
}

func TestCreationOnAMissingParentFails(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("GuildChannelCreate", &discordapi.Error{Status: 404, Code: discordapi.CodeUnknownChannel})
	res, err := f.run(task(), store.Channel{})
	require.NoError(t, err)
	assert.Equal(t, store.StatusFailed, res.Rec.Status)
	assert.Empty(t, res.Rec.ChannelID)
	assert.NotEmpty(t, res.Rec.LastError)
}

func TestListingFailureStopsCreation(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("GuildChannels", &discordapi.Error{Status: 500})
	_, err := f.run(task(), store.Channel{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing channels")
	assert.Zero(t, f.api.Mutations())
}

func TestCapacityNoteNamesUntitledIdentifierlessTasks(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("GuildChannelCreate", &discordapi.Error{Status: 400, Code: discordapi.CodeMaxGuildChannels})
	tk := task()
	tk.Identifier = ""
	_, err := f.run(tk, store.Channel{})
	require.NoError(t, err)
	require.Len(t, f.notes, 1)
	assert.Contains(t, f.notes[0], `for "Fix login":`)
}

func TestRetryAdoptsTheChannelALostWriteLeftBehind(t *testing.T) {
	f := newFixture(t)
	f.api.StartIDsAt(f.now)
	rec := f.creating(t, task())
	creates := len(f.api.CallsTo("GuildChannelCreate"))
	require.Equal(t, 1, creates)

	renamed := task()
	renamed.Title = "Fix login on Safari" // renamed while the record was lost
	res, err := f.run(renamed, rec)
	require.NoError(t, err)
	assert.Len(t, f.api.CallsTo("GuildChannelCreate"), 1, "no duplicate channel")
	assert.Equal(t, store.StatusActive, res.Rec.Status)
	assert.Equal(t, "Fix login", res.Rec.SyncedTitle, "the adopted channel still has the old name")
	assert.Equal(t, time.Second, res.RetryAfter, "the rename converges promptly")
	assert.Nil(t, res.Rec.Creation)
}

func TestAdoptionSkipsChannelsThatDoNotMatch(t *testing.T) {
	f := newFixture(t)
	f.api.StartIDsAt(f.now)
	rec := f.creating(t, task())
	made := f.api.CallsTo("GuildChannelCreate")
	require.Len(t, made, 1)
	var orig *discordgo.Channel
	for _, ch := range f.api.GuildChannelList(guildID) {
		if ch.Type == discordgo.ChannelTypeGuildText && ch.Name == rec.Creation.Name {
			orig = ch
		}
	}
	require.NotNil(t, orig)
	// Another task owns the channel that matches by name and topic.
	f.m.Owner = func(_ context.Context, channelID string) (string, bool, error) {
		return "other-task", channelID == orig.ID, nil
	}
	res, err := f.run(task(), rec)
	require.NoError(t, err)
	assert.NotEqual(t, orig.ID, res.Rec.ChannelID, "another task's channel is never taken")
	assert.Len(t, f.api.CallsTo("GuildChannelCreate"), 2)
}

func TestAdoptionIgnoresOlderAndMisplacedChannels(t *testing.T) {
	f := newFixture(t)
	f.api.StartIDsAt(f.now.Add(-time.Hour)) // existing channels predate the request
	want := store.Creation{Name: "kan-42-fix-login", Topic: "t", At: f.now}
	cat := f.guild.TaskCategoryIDs[0]
	f.api.AddChannel(&discordgo.Channel{GuildID: guildID, Type: discordgo.ChannelTypeGuildText, Name: want.Name, Topic: "t", ParentID: cat})
	f.api.StartIDsAt(f.now)
	f.api.AddChannel(&discordgo.Channel{GuildID: guildID, Type: discordgo.ChannelTypeGuildVoice, Name: want.Name, Topic: "t", ParentID: cat})
	f.api.AddChannel(&discordgo.Channel{GuildID: guildID, Type: discordgo.ChannelTypeGuildText, Name: "other", Topic: "t", ParentID: cat})
	f.api.AddChannel(&discordgo.Channel{GuildID: guildID, Type: discordgo.ChannelTypeGuildText, Name: want.Name, Topic: "t", ParentID: "elsewhere"})

	rec := store.Channel{TaskID: "t1", WorkspaceID: "ws", Status: store.StatusCreating, Creation: &want}
	res, err := f.run(nil, rec) // the task vanished before the record was written
	require.NoError(t, err)
	assert.Equal(t, store.StatusDeleted, res.Rec.Status, "nothing adoptable: nothing was created")
	assert.Nil(t, res.Rec.Creation)
	assert.True(t, res.Changed)
	assert.Zero(t, f.api.Mutations())
}

func TestAbandonedCreationAdoptsThenClosesTheChannel(t *testing.T) {
	f := newFixture(t)
	f.api.StartIDsAt(f.now)
	rec := f.creating(t, task())

	archived := task()
	at := "2026-11-25T12:00:00Z"
	archived.ArchivedAt = &at
	res, err := f.run(archived, rec)
	require.NoError(t, err)
	assert.Equal(t, store.StatusActive, res.Rec.Status, "adopted first, saved before closing")
	assert.NotEmpty(t, res.Rec.ChannelID)
	assert.Equal(t, rec.Creation.RolesKey, res.Rec.RolesKey)
	assert.Equal(t, f.now.Format(time.RFC3339Nano), res.Rec.OutboundCursor.CreatedAt, "nothing old is posted on close")
	assert.Equal(t, time.Second, res.RetryAfter)

	res, err = f.run(archived, res.Rec)
	require.NoError(t, err)
	assert.Equal(t, store.StatusArchived, res.Rec.Status)
}

func TestAbandonOwnershipErrorIsReturned(t *testing.T) {
	f := newFixture(t)
	f.api.StartIDsAt(f.now)
	rec := f.creating(t, task())
	f.m.Owner = func(context.Context, string) (string, bool, error) { return "", false, errors.New("state down") }
	res, err := f.run(nil, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking channel ownership")
	assert.Equal(t, rec, res.Rec)
}

func TestCurrentGuildSeesOtherTasksRollovers(t *testing.T) {
	f := newFixture(t)
	for range CategoryCapacity {
		f.api.AddChannel(&discordgo.Channel{GuildID: guildID, ParentID: f.guild.TaskCategoryIDs[0]})
	}
	cat2, err := provision.CreateCategory(context.Background(), f.api, guildID, "Kandev Tasks 2", nil)
	require.NoError(t, err)
	latest := f.guild
	latest.TaskCategoryIDs = append([]string{}, f.guild.TaskCategoryIDs[0], cat2.ID)
	var asked string
	f.m.CurrentGuild = func(ws string) (store.Guild, bool) { asked = ws; return latest, true }
	f.m.CategoryLock = &sync.Mutex{}

	res, err := f.run(task(), store.Channel{})
	require.NoError(t, err)
	assert.Equal(t, "ws", asked)
	assert.Equal(t, cat2.ID, f.api.Channels[res.Rec.ChannelID].ParentID, "no third category")
}

func TestDeletedCategoriesAreDroppedBeforePlacement(t *testing.T) {
	f := newFixture(t)
	old := f.guild.TaskCategoryIDs[0]
	delete(f.api.Channels, old)
	res, err := f.run(task(), store.Channel{})
	require.NoError(t, err)
	require.Len(t, f.guild.TaskCategoryIDs, 1)
	assert.NotEqual(t, old, f.guild.TaskCategoryIDs[0])
	assert.Equal(t, provision.TaskCategoryName, f.api.Channels[f.guild.TaskCategoryIDs[0]].Name, "first category keeps the base name")
	assert.Equal(t, f.guild.TaskCategoryIDs[0], f.api.Channels[res.Rec.ChannelID].ParentID)
}

func TestStaleSecondCategoryIsDroppedAndTheFirstReused(t *testing.T) {
	f := newFixture(t)
	f.guild.TaskCategoryIDs = append(f.guild.TaskCategoryIDs, "deleted-category")
	f.m.SaveGuild = nil
	res, err := f.run(task(), store.Channel{})
	require.NoError(t, err)
	assert.Equal(t, f.guild.TaskCategoryIDs[0], f.api.Channels[res.Rec.ChannelID].ParentID)
}

func TestCategoryCreationFailureIsReturned(t *testing.T) {
	f := newFixture(t)
	delete(f.api.Channels, f.guild.TaskCategoryIDs[0])
	f.api.FailNext("GuildChannelCreate", &discordapi.Error{Status: 500})
	_, err := f.run(task(), store.Channel{})
	require.Error(t, err)
}

func TestConvergeRewritesOverwritesForNewRoles(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	f.guild.OperatorRoleID = "new-operator-role"
	f.api.ResetCalls()

	res, err := f.m.Converge(context.Background(), Input{Task: task(), Rec: rec, Guild: f.guild, Now: f.now}, false)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, RolesKey(f.guild), res.Rec.RolesKey)
	ch := f.api.Channels[rec.ChannelID]
	assert.True(t, provision.SameOverwrites(ch.PermissionOverwrites, provision.TaskOverwrites(provision.RolesOf(f.guild, botID))))

	f.guild.ViewerRoleID = "viewer"
	res, err = f.m.Converge(context.Background(), Input{Task: task(), Rec: res.Rec, Guild: f.guild, Now: f.now}, true)
	require.NoError(t, err)
	assert.True(t, provision.SameOverwrites(f.api.Channels[rec.ChannelID].PermissionOverwrites, provision.ReadOnlyOverwrites(provision.RolesOf(f.guild, botID))))

	same, err := f.m.Converge(context.Background(), Input{Task: task(), Rec: res.Rec, Guild: f.guild, Now: f.now}, true)
	require.NoError(t, err)
	assert.False(t, same.Changed, "matching roles need no call")
	assert.Len(t, f.api.CallsTo("ChannelEdit"), 2)
}

func TestConvergeFailures(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	f.guild.OperatorRoleID = "new"
	in := Input{Task: task(), Rec: rec, Guild: f.guild, Now: f.now}

	f.api.FailNext("ChannelEdit", &discordapi.Error{Status: 500})
	res, err := f.m.Converge(context.Background(), in, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "updating channel permissions")
	assert.Equal(t, rec, res.Rec)

	delete(f.api.Channels, rec.ChannelID)
	res, err = f.m.Converge(context.Background(), in, false)
	require.NoError(t, err)
	assert.Equal(t, store.StatusDeleted, res.Rec.Status)
}

func TestActiveSyncStopsWhenConvergeFindsTheChannelGone(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	f.guild.OperatorRoleID = "new"
	delete(f.api.Channels, rec.ChannelID)
	renamed := task()
	renamed.Title = "Other"
	res := f.sync(t, renamed, rec)
	assert.Equal(t, store.StatusDeleted, res.Rec.Status)
	assert.Empty(t, f.api.CallsTo("ChannelDelete"))
}

func TestRenameFailures(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	renamed := task()
	renamed.Title = "New title"

	f.api.FailNext("ChannelEdit", &discordapi.Error{Status: 500})
	res, err := f.run(renamed, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renaming channel")
	assert.Equal(t, "Fix login", res.Rec.SyncedTitle)

	delete(f.api.Channels, rec.ChannelID)
	res, err = f.run(renamed, rec)
	require.NoError(t, err)
	assert.Equal(t, store.StatusDeleted, res.Rec.Status)
}

func TestTopicOnlyChangeKeepsTheName(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	f.api.ResetCalls()
	res, err := f.m.Sync(context.Background(), Input{Task: task(), Rec: rec, Guild: f.guild, ParentIdentifier: "KAN-1", Now: f.now})
	require.NoError(t, err)
	assert.True(t, res.Changed)
	edits := f.api.CallsTo("ChannelEdit")
	require.Len(t, edits, 1)
	edit := edits[0].Args[1].(discordgo.ChannelEdit)
	assert.Empty(t, edit.Name)
	assert.Contains(t, edit.Topic, "subtask of KAN-1")
}

func TestArchiveAndReopenFailures(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	archived := task()
	at := "2026-11-25T12:00:00Z"
	archived.ArchivedAt = &at

	f.api.FailNext("ChannelEdit", &discordapi.Error{Status: 500})
	_, err := f.run(archived, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archiving channel")

	rec = f.sync(t, archived, rec).Rec
	f.api.FailNext("ChannelEdit", &discordapi.Error{Status: 500})
	_, err = f.run(task(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reopening channel")

	delete(f.api.Channels, rec.ChannelID)
	res, err := f.run(task(), rec)
	require.NoError(t, err)
	assert.Equal(t, store.StatusDeleted, res.Rec.Status)
}

func TestArchivedChannelWaitsForRetention(t *testing.T) {
	f := newFixture(t)
	rec := f.sync(t, task(), store.Channel{}).Rec
	archived := task()
	at := "2026-11-25T12:00:00Z"
	archived.ArchivedAt = &at
	rec = f.sync(t, archived, rec).Rec

	f.now = f.now.Add(24 * time.Hour)
	res := f.sync(t, archived, rec)
	assert.False(t, res.Changed)
	assert.Equal(t, 29*24*time.Hour, res.RetryAfter)

	f.m.Config.ArchiveRetentionDays = 0
	keep := f.sync(t, task(), rec).Rec
	keep = f.sync(t, archived, keep).Rec
	assert.True(t, keep.DeleteAfter.IsZero(), "zero retention keeps archives forever")
	assert.Zero(t, f.sync(t, archived, keep).RetryAfter)
}

func TestRemoveFailureKeepsTheRecord(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.ClosedChannelAction = config.ClosedDelete })
	rec := f.sync(t, task(), store.Channel{}).Rec
	f.api.FailNext("ChannelDelete", &discordapi.Error{Status: 500})
	res, err := f.run(nil, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deleting channel (task closed)")
	assert.Equal(t, store.StatusActive, res.Rec.Status)

	delete(f.api.Channels, rec.ChannelID) // already gone counts as deleted
	res, err = f.run(nil, rec)
	require.NoError(t, err)
	assert.Equal(t, store.StatusDeleted, res.Rec.Status)
}

func TestRecordsTheManagerLeavesAlone(t *testing.T) {
	f := newFixture(t)
	lost := store.Channel{TaskID: "t1", ChannelID: "123", Status: store.StatusFailed, UpdatedAt: f.now.Add(-2 * time.Hour)}
	res := f.sync(t, task(), lost)
	assert.Equal(t, lost, res.Rec, "a channel the bot lost access to waits for Reactivate")

	failedClosed := store.Channel{TaskID: "t1", Status: store.StatusFailed, UpdatedAt: f.now.Add(-2 * time.Hour)}
	assert.Equal(t, failedClosed, f.sync(t, nil, failedClosed).Rec)

	unknown := store.Channel{TaskID: "t1", Status: "mystery"}
	assert.Equal(t, unknown, f.sync(t, task(), unknown).Rec)

	blocked := store.Channel{TaskID: "t1", Status: store.StatusCapacityBlocked}
	assert.Equal(t, blocked, f.sync(t, nil, blocked).Rec, "a closed task needs no channel")
	assert.Zero(t, f.api.Mutations())
}
