package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/commands"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/gateway"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

const operatorID = "500000000000000001"

// interact delivers one interaction from the gateway and waits for its
// answer and the work it queued.
func (h *harness) interact(t *testing.T, i *discordgo.Interaction) string {
	t.Helper()
	h.b.onInteraction(context.Background(), &discordgo.InteractionCreate{Interaction: i})
	h.wait()
	return h.api.LastReply(i.ID)
}

func (h *harness) statusCommand(channelID string) *discordgo.Interaction {
	return &discordgo.Interaction{
		ID: h.api.NewID(), Type: discordgo.InteractionApplicationCommand, GuildID: guildID, ChannelID: channelID,
		Data:   discordgo.ApplicationCommandInteractionData{Name: commands.CmdStatus},
		Member: &discordgo.Member{User: &discordgo.User{ID: operatorID, Username: "alice"}},
	}
}

func TestInteractionsAreAnsweredOnceForTheirTaskChannel(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")

	i := h.statusCommand(rec.ChannelID)
	got := h.interact(t, i)
	assert.Contains(t, got, "**KAN-1 · Fix login**")
	assert.Contains(t, got, "https://kandev.example.com/t/1")

	h.interact(t, i) // gateway redelivery
	assert.Len(t, h.api.Interactions[i.ID], 2, "one deferral and one answer: a redelivered interaction is ignored")

	h.b.onInteraction(context.Background(), nil)
	h.b.onInteraction(context.Background(), &discordgo.InteractionCreate{})
	dm := h.statusCommand(rec.ChannelID)
	dm.GuildID = ""
	h.interact(t, dm)
	assert.Empty(t, h.api.Interactions[dm.ID], "DMs are not answered")
}

func TestQueueDepthEventsReachStatus(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.host.Sessions["1"] = []pluginsdk.Session{{ID: "s1", TaskID: "1", State: "RUNNING", AgentDisplayName: "Claude"}}
	h.ready(t)
	rec := h.channel(t, "1")

	h.event(t, "q1", "message.queue.status_changed", map[string]any{"session_id": "s1", "count": float64(3)})
	assert.Contains(t, h.interact(t, h.statusCommand(rec.ChannelID)), "Queued messages: 3")

	h.event(t, "q2", "message.queue.status_changed", map[string]any{"session_id": "s1", "count": "bad"})
	assert.Contains(t, h.interact(t, h.statusCommand(rec.ChannelID)), "Queued messages: 3", "a malformed count is ignored")
}

func TestEventsMapToTheirTask(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	h.addTask("1", "Fix login")
	h.api.ResetCalls()

	h.event(t, "e1", "task_session.state_changed", map[string]any{"session_id": "unknown"})
	h.b.HandleEvent(context.Background(), nil)
	assert.Empty(t, h.api.CallsTo("GuildChannelCreate"), "an unknown session maps to no task")

	h.event(t, "e2", "task.created", map[string]any{"id": "1"})
	assert.Len(t, h.api.CallsTo("GuildChannelCreate"), 1, "task events may carry only the task's id")
}

func TestGatewayStateIsRecordedForTheSettingsCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.b.setGateway(ctx, gateway.State{Name: gateway.StateFatal, Reason: "disallowed intents (close code 4014)", CloseCode: 4014, Identifies24h: 3})
	got, ok, err := h.st.Health(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, gateway.StateFatal, got.GatewayState)
	assert.Equal(t, 4014, got.LastCloseCode)
	assert.Equal(t, 3, got.IdentifyCount24h)
	assert.Equal(t, "disallowed intents (close code 4014)", got.Reason)
	assert.Equal(t, h.clock.UTC(), got.CheckedAt)

	h.b.setGateway(ctx, gateway.State{Name: gateway.StateConnected})
	got, _, _ = h.st.Health(ctx)
	assert.Equal(t, gateway.StateConnected, got.GatewayState)
	assert.Equal(t, 4014, got.LastCloseCode, "the last close code stays visible")
	assert.Equal(t, 3, got.IdentifyCount24h)
	assert.Empty(t, got.Reason)
}

func TestGatewayPauseIsAnnouncedInStatusChannel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.b.setGateway(ctx, gateway.State{Name: gateway.StateConnected})
	h.ready(t)
	g, _ := h.b.guild(wsID)
	h.b.setGateway(ctx, gateway.State{Name: gateway.StateRefused, Reason: "identify budget exhausted"})
	h.wait()
	texts := h.texts(g.StatusChannelID)
	require.Len(t, texts, 2)
	assert.Equal(t, "🔴 The Discord gateway is paused: identify budget exhausted. It resumes automatically.", texts[1])

	h.b.setGateway(ctx, gateway.State{Name: gateway.StateFatal, Reason: "x"})
	h.wait()
	assert.Len(t, h.texts(g.StatusChannelID), 2, "one notice per outage")
}

// stepsHost counts workflow step reads.
type stepsHost struct {
	*kandev.Fake
	reads int
}

func (s *stepsHost) ListWorkflowSteps(ctx context.Context, id string) ([]pluginsdk.WorkflowStep, error) {
	s.reads++
	return s.Fake.ListWorkflowSteps(ctx, id)
}

func TestStepNamesAreCachedBriefly(t *testing.T) {
	h := newHarness(t)
	host := &stepsHost{Fake: h.host}
	h.b.host = host
	h.host.Steps["wf"] = []pluginsdk.WorkflowStep{{ID: "st1", Name: "Review"}, {ID: "st2", Name: "Done"}}
	ctx := context.Background()
	task := &pluginsdk.Task{ID: "1", WorkflowID: "wf", WorkflowStepID: "st1"}

	assert.Equal(t, "Review", h.b.stepName(ctx, task))
	task.WorkflowStepID = "st2"
	assert.Equal(t, "Done", h.b.stepName(ctx, task))
	assert.Equal(t, 1, host.reads, "served from the cache")

	h.host.Steps["wf"] = []pluginsdk.WorkflowStep{{ID: "st2", Name: "Shipped"}}
	*h.clock = h.clock.Add(stepCacheTTL + time.Second)
	assert.Equal(t, "Shipped", h.b.stepName(ctx, task), "renamed steps show after the TTL")
	assert.Equal(t, 2, host.reads)

	task.WorkflowStepID = "gone"
	assert.Empty(t, h.b.stepName(ctx, task))
	assert.Empty(t, h.b.stepName(ctx, &pluginsdk.Task{ID: "2"}), "no workflow, no read")
	assert.Equal(t, 2, host.reads)
}

func TestSubtaskTopicNamesItsParent(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	h.host.AddTask(pluginsdk.Task{ID: "p", WorkspaceID: wsID, Identifier: "KAN-P", Title: "Parent", State: "IN_PROGRESS"})
	h.host.AddTask(pluginsdk.Task{ID: "q", WorkspaceID: wsID, Title: "Untagged parent", State: "IN_PROGRESS"})
	for id, parent := range map[string]string{"c1": "p", "c2": "q", "c3": "missing"} {
		pid := parent
		h.host.AddTask(pluginsdk.Task{ID: id, WorkspaceID: wsID, Identifier: "KAN-" + id, Title: "Child", State: "IN_PROGRESS", ParentID: &pid})
		h.event(t, "e-"+id, "task.created", map[string]any{"task_id": id})
	}
	assert.Contains(t, h.api.Channels[h.channel(t, "c1").ChannelID].Topic, "subtask of KAN-P")
	assert.Contains(t, h.api.Channels[h.channel(t, "c2").ChannelID].Topic, "subtask of Untagged parent")
	assert.NotContains(t, h.api.Channels[h.channel(t, "c3").ChannelID].Topic, "subtask of", "an unreadable parent is left out")
}

func TestCreationInventoryIsSettled(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	ctx := context.Background()
	binding := config.Binding{WorkspaceID: wsID, GuildID: guildID}
	put := func(taskID, ws string) {
		require.NoError(t, h.st.PutCreating(ctx, store.Index{TaskID: taskID, WorkspaceID: ws, GuildID: guildID}))
	}
	put("norecord", wsID)
	put("failed", wsID)
	require.NoError(t, h.st.PutChannel(ctx, store.Channel{TaskID: "failed", WorkspaceID: wsID, Status: store.StatusFailed}, ""))
	put("archived-unindexed", wsID)
	require.NoError(t, h.st.PutChannel(ctx, store.Channel{TaskID: "archived-unindexed", WorkspaceID: wsID, Status: store.StatusArchived}, ""))
	put("archived", wsID)
	require.NoError(t, h.st.PutChannel(ctx, store.Channel{TaskID: "archived", WorkspaceID: wsID, GuildID: guildID, ChannelID: "777", Status: store.StatusArchived}, ""))
	put("listed", wsID)
	put("elsewhere", "ws-2")

	h.b.reconcileCreations(ctx, binding, map[string]bool{"listed": true})
	h.wait()
	left, err := h.st.CreatingTasks(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"listed", "elsewhere"}, keys(left), "settled entries leave the inventory")
}

func TestUnsettledCreationIsSyncedAgain(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	ctx := context.Background()
	h.api.StartIDsAt(*h.clock)
	// A creation whose task vanished: the record still says creating.
	require.NoError(t, h.st.PutCreating(ctx, store.Index{TaskID: "gone", WorkspaceID: wsID, GuildID: guildID}))
	require.NoError(t, h.st.PutChannel(ctx, store.Channel{TaskID: "gone", WorkspaceID: wsID, GuildID: guildID, Status: store.StatusCreating,
		Creation: &store.Creation{Name: "kan-gone", Topic: "t", At: *h.clock}}, ""))

	h.b.reconcileCreations(ctx, config.Binding{WorkspaceID: wsID, GuildID: guildID}, map[string]bool{})
	h.wait()
	assert.Equal(t, store.StatusDeleted, h.channel(t, "gone").Status, "nothing was created; the record settles")
	left, _ := h.st.CreatingTasks(ctx)
	assert.Empty(t, left)
}

func TestChannelDeleteOfAnUnknownChannelChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	before := h.channel(t, "1")
	h.b.onChannelDelete(context.Background(), &discordgo.Channel{ID: "123"})
	h.wait()
	assert.Equal(t, before, h.channel(t, "1"))
	assert.False(t, h.b.markGone(context.Background(), "1", "123"), "a stale channel ID does not delete the record")
	assert.False(t, h.b.markGone(context.Background(), "nope", before.ChannelID))
}

func TestBridgeDefaultsLetEventsRun(t *testing.T) {
	cfg, err := config.Parse(map[string]any{"bot_token": "x", "application_id": appID, "workspace_guilds": wsID + "=" + guildID})
	require.NoError(t, err)
	b := New(Deps{Config: cfg, API: nil, Host: kandev.NewFake(), Store: store.New(store.NewMemoryHost())})
	assert.False(t, b.halted())
	assert.WithinDuration(t, time.Now(), b.now(), time.Minute)
	assert.Equal(t, gateway.StateStopped, b.health.GatewayState)
	require.Len(t, b.health.Bindings, 1)
	assert.Equal(t, guildID, b.health.Bindings[0].GuildID)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
