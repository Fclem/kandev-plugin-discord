package bridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

// flakyState fails the Nth write of one key, and the next reads of
// getFails keys.
type flakyState struct {
	*store.MemoryHost
	key   string
	fail  int32
	count atomic.Int32

	mu       sync.Mutex
	getFails map[string]int
}

func (f *flakyState) SetState(ctx context.Context, scope, scopeID, key string, v map[string]any) error {
	if scope+"/"+scopeID+"/"+key == f.key && f.count.Add(1) == f.fail {
		return errors.New("host unavailable")
	}
	return f.MemoryHost.SetState(ctx, scope, scopeID, key, v)
}

func (f *flakyState) GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	f.mu.Lock()
	k := scope + "/" + scopeID + "/" + key
	if f.getFails[k] > 0 {
		f.getFails[k]--
		f.mu.Unlock()
		return nil, false, errors.New("host unavailable")
	}
	f.mu.Unlock()
	return f.MemoryHost.GetState(ctx, scope, scopeID, key)
}

// failReads makes the next n reads of a state key fail.
func (f *flakyState) failReads(key string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getFails == nil {
		f.getFails = map[string]int{}
	}
	f.getFails[key] += n
}

// withFlakyStore rebuilds the harness bridge over a flakyState.
func (h *harness) withFlakyStore() *flakyState {
	flaky := &flakyState{MemoryHost: h.mem}
	h.st = store.New(flaky)
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	return flaky
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
	h.b.ensureCaughtUp(ctx, rec, g, "")

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

	binding := config.Binding{WorkspaceID: wsID, GuildID: guildID}
	h.b.provisionGuild(ctx, binding) // REST re-provisioning while the gateway is still down
	assert.Len(t, notes(), 2, "no false all-clear")

	h.api.FailNext("GuildRoles", &discordapi.Error{Status: 503})
	h.b.provisionGuild(ctx, binding)
	require.Len(t, notes(), 3)
	assert.Contains(t, notes()[2], "Provisioning failed")
	h.b.provisionGuild(ctx, binding)
	require.Len(t, notes(), 4)
	assert.Contains(t, notes()[3], "Provisioning succeeded")
	assert.NotContains(t, notes()[3], "Reconnected", "the gateway outage is still on")

	h.b.setGateway(ctx, gateway.State{Name: gateway.StateConnected})
	h.ready(t)
	require.Len(t, notes(), 5)
	assert.Contains(t, notes()[4], "Reconnected")
}

func TestUnreadableRouteRetriesFromHistoryBeforeLaterMessages(t *testing.T) {
	h := newHarness(t)
	flaky := h.withFlakyStore()
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	member := &discordgo.Member{Roles: []string{g.OperatorRoleID}}
	author := &discordgo.User{ID: "500000000000000001", Username: "alice"}
	h.api.Members[author.ID] = member
	ctx := context.Background()
	h.b.ensureCaughtUp(ctx, rec, g, "")

	at := *h.clock
	first := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(at.Add(-2 * time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "first", Author: author, Member: member})
	flaky.failReads("instance//channel:"+rec.ChannelID, 1)
	h.b.onMessageCreate(ctx, &discordgo.MessageCreate{Message: first})
	h.wait()
	require.Empty(t, h.host.Sent)

	second := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(at.Add(-time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "second", Author: author, Member: member})
	h.b.onMessageCreate(ctx, &discordgo.MessageCreate{Message: second})
	h.wait()
	require.Len(t, h.host.Sent, 2)
	assert.Equal(t, "alice (Discord): first", h.host.Sent[0].Text)
	assert.Equal(t, "alice (Discord): second", h.host.Sent[1].Text)
}

func TestAdoptedChannelConvergesRolesBeforeAnyPost(t *testing.T) {
	h := newHarness(t)
	h.api.StartIDsAt(*h.clock)
	flaky := &flakyState{MemoryHost: h.mem, key: "task/1/channel", fail: 2} // the write after creation fails
	h.st = store.New(flaky)
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.addTask("1", "Fix login")
	h.ready(t)
	g, _ := h.b.guild(wsID)
	oldRole := g.OperatorRoleID
	g.OperatorRoleID = h.api.NewID() // authority moved before the retry
	require.NoError(t, h.b.setGuild(context.Background(), wsID, g))
	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", AuthorType: "agent", Type: "message", Content: "secret answer", CreatedAt: "2026-11-25T12:00:01Z"})

	h.reconcile(t) // adopts the channel made with the former role's overwrites
	rec := h.channel(t, "1")
	require.Equal(t, store.StatusActive, rec.Status)
	assert.NotContains(t, h.texts(rec.ChannelID), "secret answer")

	h.reconcile(t)
	assert.Contains(t, h.texts(rec.ChannelID), "secret answer")
	for _, ow := range h.api.Channels[rec.ChannelID].PermissionOverwrites {
		assert.NotEqual(t, oldRole, ow.ID, "the former operator role lost its overwrite")
	}
}

func TestFailedReprovisioningStillMovesAuthority(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	old, _ := h.b.guild(wsID)
	rec := h.channel(t, "1")

	h.b.cfg.OperatorRoleName = "Kandev Ops"
	h.api.FailNext("ChannelEdit", &discordapi.Error{Status: 503}) // the category overwrite fix fails
	h.ready(t)
	g, _ := h.b.guild(wsID)
	require.NotEqual(t, old.OperatorRoleID, g.OperatorRoleID, "the new role is trusted despite the failure")
	assert.Equal(t, channels.RolesKey(g), h.channel(t, "1").RolesKey, "the task channel converged")
	for _, ow := range h.api.Channels[rec.ChannelID].PermissionOverwrites {
		assert.NotEqual(t, old.OperatorRoleID, ow.ID)
	}
}

func TestCloseWaitsForARunningSession(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CloseTrigger = config.CloseOnTerminal })
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	h.host.Tasks["1"].State = "COMPLETED"
	h.host.Tasks["1"].UpdatedAt = h.clock.Format(time.RFC3339)
	h.host.Sessions["1"] = []pluginsdk.Session{{ID: "s1", TaskID: "1", State: "RUNNING"}}
	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", SessionID: "s1", AuthorType: "agent", Type: "message", Content: "Final answer, still streaming", CreatedAt: "2026-11-25T12:00:01Z"})
	h.event(t, "e1", "task.state_changed", map[string]any{"task_id": "1"})
	assert.Equal(t, store.StatusActive, h.channel(t, "1").Status, "the session's last message may still grow")

	h.host.Sessions["1"][0].State = "COMPLETED"
	h.event(t, "e2", "task_session.state_changed", map[string]any{"task_id": "1"})
	assert.Contains(t, h.texts(rec.ChannelID), "Final answer, still streaming")
	assert.Equal(t, store.StatusArchived, h.channel(t, "1").Status)
}

func TestTaskClosedDuringAnUnrecordedCreationClosesItsChannel(t *testing.T) {
	h := newHarness(t)
	h.api.StartIDsAt(*h.clock)
	flaky := &flakyState{MemoryHost: h.mem, key: "task/1/channel", fail: 2}
	h.st = store.New(flaky)
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.addTask("1", "Fix login")
	h.ready(t)
	require.Equal(t, store.StatusCreating, h.channel(t, "1").Status)

	archived := h.clock.Format(time.RFC3339)
	h.host.Tasks["1"].ArchivedAt = &archived
	h.reconcile(t) // adopts the unrecorded channel and saves that first
	assert.Equal(t, store.StatusActive, h.channel(t, "1").Status)
	h.reconcile(t) // then closes it like any other
	rec := h.channel(t, "1")
	assert.Equal(t, store.StatusArchived, rec.Status)
	require.NotEmpty(t, rec.ChannelID)
	g, _ := h.b.guild(wsID)
	assert.Contains(t, g.ArchiveCategoryIDs, h.api.Channels[rec.ChannelID].ParentID)
	assert.Len(t, h.api.CallsTo("GuildChannelCreate"), 3+1, "nothing new was created")
}

func TestTaskDeletedDuringAnUnrecordedCreationClosesItsChannel(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ClosedChannelAction = config.ClosedDelete })
	h.api.StartIDsAt(*h.clock)
	flaky := &flakyState{MemoryHost: h.mem, key: "task/1/channel", fail: 2}
	h.st = store.New(flaky)
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.addTask("1", "Fix login")
	h.ready(t)
	require.Equal(t, store.StatusCreating, h.channel(t, "1").Status)
	var made string
	for id, ch := range h.api.Channels {
		if ch.Name == "kan-1-fix-login" {
			made = id
		}
	}
	require.NotEmpty(t, made)

	delete(h.host.Tasks, "1") // hard-deleted while the bridge was down
	h.reconcile(t)
	h.reconcile(t)
	assert.NotContains(t, h.api.Channels, made, "the unrecorded channel was deleted")
	assert.Equal(t, store.StatusDeleted, h.channel(t, "1").Status)
	pending, err := h.st.CreatingTasks(context.Background())
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestArchivedChannelPromptsLoseTheirButtonsEventually(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.host.Interactions["p1"] = &pluginsdk.Interaction{ID: "p1", Kind: pluginsdk.InteractionKindPermission, TaskID: "1",
		Status: pluginsdk.InteractionStatusPending, Title: "Run tests?",
		Options: []pluginsdk.InteractionOption{{OptionID: "a", Label: "Allow", Kind: "allow_once"}}}
	h.ready(t)
	rec := h.channel(t, "1")
	var prompt *discordgo.Message
	for _, m := range h.api.SentMessages(rec.ChannelID) {
		if strings.Contains(m.Content, "Run tests?") {
			prompt = m
		}
	}
	require.NotNil(t, prompt)
	require.NotEmpty(t, prompt.Components)

	archived := h.clock.Format(time.RFC3339)
	h.host.Tasks["1"].ArchivedAt = &archived
	h.api.FailNext("MessageEdit", &discordapi.Error{Status: 503})
	h.event(t, "e1", "task.updated", map[string]any{"task_id": "1"})
	require.Equal(t, store.StatusArchived, h.channel(t, "1").Status)
	assert.NotEmpty(t, h.api.Message(rec.ChannelID, prompt.ID).Components, "the edit failed once")

	h.reconcile(t)
	assert.Empty(t, h.api.Message(rec.ChannelID, prompt.ID).Components, "retried on a later sync")
	stored, err := h.st.Prompts(context.Background(), "1")
	require.NoError(t, err)
	assert.Empty(t, stored.Items)
}

func TestLiveMessageIsNotTakenForMissedHistory(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.InboundCatchupMaxAge = 0 }) // missed messages are never delivered
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	member := &discordgo.Member{Roles: []string{g.OperatorRoleID}}
	author := &discordgo.User{ID: "500000000000000001", Username: "alice"}
	h.api.Members[author.ID] = member

	live := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(h.clock.Add(-time.Second)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "go", Author: author, Member: member})
	h.b.onMessageCreate(context.Background(), &discordgo.MessageCreate{Message: live}) // already in REST history
	h.wait()
	require.Len(t, h.host.Sent, 1, "the live message is delivered, not refused as offline history")
	assert.Equal(t, "alice (Discord): go", h.host.Sent[0].Text)
}

func TestMissingChannelIndexIsRepairedAndCaughtUp(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	author := &discordgo.User{ID: "500000000000000001", Username: "alice"}
	h.api.Members[author.ID] = &discordgo.Member{Roles: []string{g.OperatorRoleID}}
	ctx := context.Background()
	require.NoError(t, h.mem.DeleteState(ctx, "instance", "", "channel:"+rec.ChannelID)) // its write failed
	m := h.api.AddMessage(&discordgo.Message{ID: snowflakeAt(h.clock.Add(-time.Minute)), ChannelID: rec.ChannelID, GuildID: guildID, Content: "unroutable", Author: author})
	h.b.onMessageCreate(ctx, &discordgo.MessageCreate{Message: m})
	h.wait()
	require.Empty(t, h.host.Sent)

	h.reconcile(t)
	h.wait()
	ix, found, err := h.st.ChannelIndex(ctx, rec.ChannelID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "1", ix.TaskID)
	require.Len(t, h.host.Sent, 1, "the message posted while unroutable is caught up")
	assert.Equal(t, "alice (Discord): unroutable", h.host.Sent[0].Text)
}

func TestCloseHoldIsBoundedFromTheFirstAttempt(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CloseTrigger = config.CloseOnTerminal })
	h.addTask("1", "Fix login")
	h.ready(t)
	h.host.Tasks["1"].State = "COMPLETED"
	h.host.Sessions["1"] = []pluginsdk.Session{{ID: "s1", TaskID: "1", State: "RUNNING"}} // never stops
	for i := range 4 {
		h.host.Tasks["1"].UpdatedAt = h.clock.Format(time.RFC3339) // renamed before each attempt
		h.event(t, fmt.Sprint("e", i), "task.updated", map[string]any{"task_id": "1"})
		require.Equal(t, store.StatusActive, h.channel(t, "1").Status)
		*h.clock = h.clock.Add(10 * time.Minute)
	}
	h.event(t, "e4", "task.updated", map[string]any{"task_id": "1"})
	assert.Equal(t, store.StatusArchived, h.channel(t, "1").Status, "40 minutes after the first attempt")
}

// lostIndexState loses the second write of task 1's record and the first
// write of any channel index entry.
type lostIndexState struct {
	*store.MemoryHost
	taskWrites, indexWrites int
}

func (f *lostIndexState) SetState(ctx context.Context, scope, scopeID, key string, v map[string]any) error {
	if scope == "task" && scopeID == "1" && key == "channel" {
		if f.taskWrites++; f.taskWrites == 2 {
			return errors.New("lost record write")
		}
	}
	if scope == "instance" && strings.HasPrefix(key, "channel:") {
		if f.indexWrites++; f.indexWrites == 1 {
			return errors.New("lost index write")
		}
	}
	return f.MemoryHost.SetState(ctx, scope, scopeID, key, v)
}

func TestDeletedTaskChannelIsClosedEvenIfItsAdoptionLostTheIndex(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ClosedChannelAction = config.ClosedDelete })
	h.api.StartIDsAt(*h.clock)
	h.st = store.New(&lostIndexState{MemoryHost: h.mem})
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.addTask("1", "One")
	h.ready(t)
	require.Equal(t, store.StatusCreating, h.channel(t, "1").Status)
	var made string
	for id, ch := range h.api.Channels {
		if ch.Name == "kan-1-one" {
			made = id
		}
	}
	require.NotEmpty(t, made)

	delete(h.host.Tasks, "1")
	h.reconcile(t) // adopts it; the record is saved, its index entry is lost
	require.Equal(t, store.StatusActive, h.channel(t, "1").Status)
	h.reconcile(t)
	h.reconcile(t)
	assert.NotContains(t, h.api.Channels, made, "the channel is still found and closed")
	pending, err := h.st.CreatingTasks(context.Background())
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// lostCreatingDelete fails the first delete of task 1's creation entry.
type lostCreatingDelete struct {
	*store.MemoryHost
	failed bool
}

func (f *lostCreatingDelete) DeleteState(ctx context.Context, scope, scopeID, key string) error {
	if key == "creating:1" && !f.failed {
		f.failed = true
		return errors.New("lost delete")
	}
	return f.MemoryHost.DeleteState(ctx, scope, scopeID, key)
}

func TestFailedCreationOfAVanishedTaskLeavesTheInventory(t *testing.T) {
	h := newHarness(t)
	h.st = store.New(&lostCreatingDelete{MemoryHost: h.mem})
	h.b = New(Deps{Config: h.b.cfg, API: h.api, Host: h.host, Store: h.st, Now: h.b.now})
	h.ready(t)
	h.addTask("1", "One")
	h.api.FailNext("GuildChannelCreate", &discordapi.Error{Status: 403, Code: 50013})
	h.reconcile(t)
	require.Equal(t, store.StatusFailed, h.channel(t, "1").Status)

	delete(h.host.Tasks, "1")
	h.reconcile(t)
	pending, err := h.st.CreatingTasks(context.Background())
	require.NoError(t, err)
	assert.Empty(t, pending, "a failed creation is settled")
}
