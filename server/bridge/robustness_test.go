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

	"kandev-plugin-discord/server/channels"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/gateway"
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

func TestLostRecordWriteAdoptsTheCreatedChannelEvenAfterARename(t *testing.T) {
	h := newHarness(t)
	h.api.StartIDsAt(*h.clock)
	flaky := &flakyState{MemoryHost: h.mem, key: "task/1/channel", fail: 2} // 1 = intent, 2 = after creation
	h.st = store.New(flaky)
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.addTask("1", "Fix login")
	h.ready(t)
	rec, found, err := h.st.Channel(context.Background(), "1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store.StatusCreating, rec.Status)
	require.NotNil(t, rec.Creation)

	h.host.Tasks["1"].Title = "Fix login flow" // renamed before the retry
	h.reconcile(t)
	rec = h.channel(t, "1")
	assert.Equal(t, store.StatusActive, rec.Status)
	assert.Nil(t, rec.Creation)
	assert.Len(t, h.api.CallsTo("GuildChannelCreate"), 3+1, "provisioning's three plus one task channel")
	h.reconcile(t) // the follow-up sync renames it
	assert.Equal(t, "kan-1-fix-login-flow", h.api.Channels[rec.ChannelID].Name, "the adopted channel is renamed")
}

func TestAdoptionSkipsChannelsOfOtherTasksAndOlderLookAlikes(t *testing.T) {
	h := newHarness(t)
	h.api.StartIDsAt(h.clock.Add(-time.Hour))
	h.ready(t)
	g, _ := h.b.guild(wsID)
	h.addTask("1", "Fix login")
	task := h.host.Tasks["1"]
	topic := channels.Topic(task, "", h.b.cfg.TaskURL("1"))
	old := h.api.AddChannel(&discordgo.Channel{GuildID: guildID, Name: "kan-1-fix-login", Topic: topic, ParentID: g.TaskCategoryIDs[0], Type: discordgo.ChannelTypeGuildText})
	h.api.StartIDsAt(*h.clock)
	owned := h.api.AddChannel(&discordgo.Channel{GuildID: guildID, Name: "kan-1-fix-login", Topic: topic, ParentID: g.TaskCategoryIDs[0], Type: discordgo.ChannelTypeGuildText})
	require.NoError(t, h.st.PutChannel(context.Background(), store.Channel{TaskID: "2", WorkspaceID: wsID, GuildID: guildID, ChannelID: owned.ID, Status: store.StatusActive}, ""))
	require.NoError(t, h.st.PutChannel(context.Background(), store.Channel{TaskID: "1", WorkspaceID: wsID, GuildID: guildID, Status: store.StatusCreating,
		Creation: &store.Creation{Name: "kan-1-fix-login", Topic: topic, Title: "Fix login", At: *h.clock}}, ""))

	h.reconcile(t)
	rec := h.channel(t, "1")
	assert.NotEqual(t, old.ID, rec.ChannelID)
	assert.NotEqual(t, owned.ID, rec.ChannelID)
	assert.Equal(t, store.StatusActive, rec.Status)
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

func TestNothingIsPostedUntilARoleChangeReachesTheChannel(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	g.OperatorRoleID = h.api.NewID() // provisioning moved authority to another role
	require.NoError(t, h.b.setGuild(context.Background(), wsID, g))
	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", AuthorType: "agent", Type: "message", Content: "secret answer", CreatedAt: "2026-11-25T12:00:01Z"})

	h.api.FailNext("ChannelEdit", &discordapi.Error{Status: 503})
	h.event(t, "e1", "turn.completed", map[string]any{"task_id": "1"})
	assert.NotContains(t, h.texts(rec.ChannelID), "secret answer", "the former role can still read the channel")

	h.event(t, "e2", "turn.completed", map[string]any{"task_id": "1"})
	assert.Contains(t, h.texts(rec.ChannelID), "secret answer")
	assert.Equal(t, channels.RolesKey(g), h.channel(t, "1").RolesKey)
}

func TestFailedFinalPostHoldsTheCloseUntilItPosts(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CloseTrigger = config.CloseOnTerminal })
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	h.host.Tasks["1"].State = "COMPLETED"
	h.host.Sessions["1"] = []pluginsdk.Session{{ID: "s1", TaskID: "1", State: "COMPLETED"}}
	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", SessionID: "s1", AuthorType: "agent", Type: "message", Content: "Done: fixed.", CreatedAt: "2026-11-25T12:00:01Z"})

	h.api.FailNext("MessageSend", &discordapi.Error{Status: 503})
	h.event(t, "e1", "task.state_changed", map[string]any{"task_id": "1"})
	assert.Equal(t, store.StatusActive, h.channel(t, "1").Status, "the close waits for the final answer")
	assert.NotContains(t, h.texts(rec.ChannelID), "Done: fixed.")

	h.event(t, "e2", "task.state_changed", map[string]any{"task_id": "1"})
	assert.Equal(t, []string{"Done: fixed."}, h.texts(rec.ChannelID))
	assert.Equal(t, store.StatusArchived, h.channel(t, "1").Status)
}

func TestFailedLiveDeliveryIsRetriedBeforeLaterMessages(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	member := &discordgo.Member{Roles: []string{g.OperatorRoleID}}
	author := &discordgo.User{ID: "500000000000000001", Username: "alice"}
	h.api.Members[author.ID] = member
	ctx := context.Background()
	h.b.ensureCaughtUp(ctx, rec, g)

	at := *h.clock
	first := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(at.Add(-2 * time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "first", Author: author})
	h.api.FailNext("GuildMember", &discordapi.Error{Status: 503}) // authority unknown for now
	h.b.onMessageCreate(ctx, &discordgo.MessageCreate{Message: first})
	h.wait()
	require.Empty(t, h.host.Sent)

	second := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(at.Add(-time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "second", Author: author, Member: member})
	h.b.onMessageCreate(ctx, &discordgo.MessageCreate{Message: second})
	h.wait()
	require.Len(t, h.host.Sent, 2, "the failed message is delivered first, from history")
	assert.Equal(t, "alice (Discord): first", h.host.Sent[0].Text)
	assert.Equal(t, "alice (Discord): second", h.host.Sent[1].Text)
}

func TestCatchUpMissedAtReadyRunsOnTheNextReconcile(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	author := &discordgo.User{ID: "500000000000000001", Username: "alice"}
	h.api.Members[author.ID] = &discordgo.Member{Roles: []string{g.OperatorRoleID}}
	h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(h.clock.Add(-time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "while offline", Author: author})

	// A reconnect whose reconcile fails after provisioning succeeded.
	ctx := context.Background()
	h.b.onReady(ctx, &discordgo.Ready{User: &discordgo.User{ID: botID}, Guilds: []*discordgo.Guild{{ID: guildID}}})
	h.api.FailNext("GuildChannels", nil)
	h.api.FailNext("GuildChannels", &discordapi.Error{Status: 503})
	h.b.onGuildCreate(ctx, &discordgo.Guild{ID: guildID})
	h.wait()
	require.Empty(t, h.host.Sent)

	h.reconcile(t)
	h.wait()
	require.Len(t, h.host.Sent, 1)
	assert.Equal(t, "alice (Discord): while offline", h.host.Sent[0].Text)
}

func TestDeletedTaskCategoryIsReplacedOnCreation(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	g, _ := h.b.guild(wsID)
	delete(h.api.Channels, g.TaskCategoryIDs[0])
	h.addTask("1", "Fix login")
	h.event(t, "e1", "task.created", map[string]any{"task_id": "1"})
	rec := h.channel(t, "1")
	require.Equal(t, store.StatusActive, rec.Status)
	g, _ = h.b.guild(wsID)
	require.Len(t, g.TaskCategoryIDs, 1)
	assert.Equal(t, g.TaskCategoryIDs[0], h.api.Channels[rec.ChannelID].ParentID)
	assert.Equal(t, "Kandev Tasks", h.api.Channels[g.TaskCategoryIDs[0]].Name)
}

func TestStatusChannelAnnouncesLifecycleTransitions(t *testing.T) {
	h := newHarness(t)
	h.b.downNoticeDelay = 0
	ctx := context.Background()
	h.b.setGateway(ctx, gateway.State{Name: gateway.StateConnected})
	h.ready(t)
	g, _ := h.b.guild(wsID)
	notes := func() []string { return h.texts(g.StatusChannelID) }
	require.Len(t, notes(), 1)
	assert.Contains(t, notes()[0], "Connected")

	h.ready(t) // routine re-provisioning is silent
	assert.Len(t, notes(), 1)

	h.b.setGateway(ctx, gateway.State{Name: gateway.StateReconnecting})
	// The timer posts from its own goroutine; poll without Wait, which must
	// not race a concurrent Do.
	require.Eventually(t, func() bool { return len(notes()) == 2 }, 2*time.Second, 10*time.Millisecond)
	h.wait()
	assert.Contains(t, notes()[1], "Disconnected")

	h.b.setGateway(ctx, gateway.State{Name: gateway.StateConnected})
	h.ready(t)
	require.Len(t, notes(), 3)
	assert.Contains(t, notes()[2], "Reconnected")
}
