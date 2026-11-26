package bridge

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/store"
)

const otherGuild = "323456789012345678"

func TestRebindingMovesTasksToAFreshChannelInTheNewGuild(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	old := h.channel(t, "1")

	// Restart with the workspace bound to another guild; same Host state.
	cfg := h.b.cfg
	cfg.Bindings = []config.Binding{{WorkspaceID: wsID, GuildID: otherGuild}}
	b2 := New(Deps{Config: cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.b = b2
	ctx := context.Background()
	b2.onReady(ctx, &discordgo.Ready{User: &discordgo.User{ID: botID}, Guilds: []*discordgo.Guild{{ID: otherGuild}}})
	b2.onGuildCreate(ctx, &discordgo.Guild{ID: otherGuild})
	h.wait()

	rec := h.channel(t, "1")
	assert.Equal(t, otherGuild, rec.GuildID)
	assert.NotEqual(t, old.ChannelID, rec.ChannelID)
	assert.Equal(t, otherGuild, h.api.Channels[rec.ChannelID].GuildID)
	_, indexed, _ := h.st.ChannelIndex(ctx, old.ChannelID)
	assert.False(t, indexed, "the old guild's channel no longer routes to the task")

	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", AuthorType: "agent", Type: "message", Content: "private output", CreatedAt: "2026-11-25T12:00:01Z"})
	h.event(t, "e1", "turn.completed", map[string]any{"task_id": "1"})
	assert.Empty(t, h.texts(old.ChannelID), "nothing new reaches the old guild")
	assert.Contains(t, h.texts(rec.ChannelID), "private output")
}

func TestMissedMessagesAreDeliveredBeforeALiveOne(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	member := &discordgo.Member{Roles: []string{g.OperatorRoleID}}
	author := &discordgo.User{ID: "500000000000000001", Username: "alice"}
	h.api.Members[author.ID] = member

	// A message posted while offline is in history; a newer one arrives live.
	at := *h.clock
	h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(at.Add(-2 * time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "first", Author: author})
	live := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(at.Add(-time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "second", Author: author, Member: member})
	h.b.mu.Lock()
	h.b.readyGen++ // a reconnect: the channel has not been caught up yet
	h.b.mu.Unlock()
	h.b.onMessageCreate(context.Background(), &discordgo.MessageCreate{Message: live})
	h.wait()

	require.Len(t, h.host.Sent, 2)
	assert.Equal(t, "alice (Discord): first", h.host.Sent[0].Text)
	assert.Equal(t, "alice (Discord): second", h.host.Sent[1].Text)
	assert.Equal(t, live.ID, h.channel(t, "1").InboundCursor)
}

// snowflakeAt builds a Discord ID created at t.
func snowflakeAt(t time.Time) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-1420070400000)<<22, 10)
}

func TestProvisioningBeforeReadyIsRetried(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.b.onGuildCreate(ctx, &discordgo.Guild{ID: guildID}) // no bot identity yet
	h.wait()
	_, ok := h.b.guild(wsID)
	require.False(t, ok)
	assert.Empty(t, h.api.CallsTo("GuildChannelCreate"), "no overwrite with an empty member ID")

	h.b.onReady(ctx, &discordgo.Ready{User: &discordgo.User{ID: botID}, Guilds: []*discordgo.Guild{{ID: guildID}}})
	h.b.reconcileAll()
	h.wait()
	_, ok = h.b.guild(wsID)
	assert.True(t, ok, "the periodic pass provisions the guild")
}

func TestChannelDeletedWhileOfflineIsRecreatedWithItsPrompts(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.host.Interactions["p1"] = &pluginsdk.Interaction{ID: "p1", Kind: pluginsdk.InteractionKindPermission, TaskID: "1",
		Status: pluginsdk.InteractionStatusPending, Title: "Run tests?",
		Options: []pluginsdk.InteractionOption{{OptionID: "a", Label: "Allow", Kind: "allow_once"}}}
	h.ready(t)
	old := h.channel(t, "1")
	require.Len(t, h.api.SentMessages(old.ChannelID), 2, "card and prompt")

	delete(h.api.Channels, old.ChannelID) // no CHANNEL_DELETE event
	h.reconcile(t)

	rec := h.channel(t, "1")
	require.NotEqual(t, old.ChannelID, rec.ChannelID)
	assert.Equal(t, store.StatusActive, rec.Status)
	var prompt bool
	for _, m := range h.api.SentMessages(rec.ChannelID) {
		prompt = prompt || strings.Contains(m.Content, "Run tests?")
	}
	assert.True(t, prompt, "the pending request is posted again in the new channel")
}

func TestConcurrentTasksShareOneRolloverCategory(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	g, _ := h.b.guild(wsID)
	for range 50 {
		h.api.AddChannel(&discordgo.Channel{GuildID: guildID, ParentID: g.TaskCategoryIDs[0]})
	}
	for _, id := range []string{"1", "2", "3"} {
		h.addTask(id, "Task "+id)
	}
	h.reconcile(t)
	g, _ = h.b.guild(wsID)
	assert.Len(t, g.TaskCategoryIDs, 2)
	n := 0
	for _, ch := range h.api.GuildChannelList(guildID) {
		if ch.Name == "Kandev Tasks 2" {
			n++
		}
	}
	assert.Equal(t, 1, n)
}

// flakyState fails the Nth write of one key.
type flakyState struct {
	*store.MemoryHost
	key   string
	fail  int32
	count atomic.Int32
}

func (f *flakyState) SetState(ctx context.Context, scope, scopeID, key string, v map[string]any) error {
	if scope+"/"+scopeID+"/"+key == f.key && f.count.Add(1) == f.fail {
		return errors.New("host unavailable")
	}
	return f.MemoryHost.SetState(ctx, scope, scopeID, key, v)
}

func TestLostRecordWriteAdoptsTheCreatedChannel(t *testing.T) {
	h := newHarness(t)
	flaky := &flakyState{MemoryHost: h.mem, key: "task/1/channel", fail: 2} // 1 = intent, 2 = after creation
	h.st = store.New(flaky)
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.addTask("1", "Fix login")
	h.ready(t)
	rec, found, err := h.st.Channel(context.Background(), "1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store.StatusCreating, rec.Status)

	h.reconcile(t)
	rec = h.channel(t, "1")
	assert.Equal(t, store.StatusActive, rec.Status)
	assert.Len(t, h.api.CallsTo("GuildChannelCreate"), 3+1, "provisioning's three plus one task channel")
}

func TestTerminalTaskPostsItsFinalAnswerBeforeClosing(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CloseTrigger = config.CloseOnTerminal })
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	h.host.Tasks["1"].State = "COMPLETED"
	h.host.Sessions["1"] = []pluginsdk.Session{{ID: "s1", TaskID: "1", State: "COMPLETED"}}
	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", SessionID: "s1", AuthorType: "agent", Type: "message", Content: "Done: fixed.", CreatedAt: "2026-11-25T12:00:01Z"})
	h.event(t, "e1", "task.state_changed", map[string]any{"task_id": "1"})
	assert.Contains(t, h.texts(rec.ChannelID), "Done: fixed.")
	assert.Equal(t, store.StatusArchived, h.channel(t, "1").Status)
}

func TestRoleChangeConvergesChannelOverwrites(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	oldRole := rec.RolesKey

	cfg := h.b.cfg
	cfg.OperatorRoleName = "Kandev Ops"
	h.b = New(Deps{Config: cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.ready(t)
	g, _ := h.b.guild(wsID)
	rec = h.channel(t, "1")
	assert.NotEqual(t, oldRole, rec.RolesKey)
	var hasNew bool
	for _, ow := range h.api.Channels[rec.ChannelID].PermissionOverwrites {
		hasNew = hasNew || ow.ID == g.OperatorRoleID
		assert.NotEqual(t, strings.Split(oldRole, "/")[0], ow.ID, "the former operator role lost its overwrite")
	}
	assert.True(t, hasNew)
}
