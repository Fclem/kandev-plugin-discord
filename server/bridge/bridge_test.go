package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

const (
	appID   = "123456789012345678"
	guildID = "223456789012345678"
	wsID    = "ws-1"
	botID   = "900000000000000001"
)

type harness struct {
	b     *Bridge
	api   *discordapi.Fake
	host  *kandev.Fake
	mem   *store.MemoryHost
	st    *store.Store
	clock *time.Time
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg, err := config.Parse(map[string]any{
		"bot_token": "MTIz.secret", "application_id": appID, "workspace_guilds": wsID + "=" + guildID,
		"kandev_base_url": "https://kandev.example.com",
	})
	require.NoError(t, err)
	for _, m := range mutate {
		m(&cfg)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	h := &harness{api: discordapi.NewFake(), host: kandev.NewFake(), mem: store.NewMemoryHost(), clock: &now}
	h.st = store.New(h.mem)
	h.b = New(Deps{Config: cfg, API: h.api, Host: h.host, Store: h.st, Now: func() time.Time { return *h.clock }})
	h.b.bind(context.Background())
	return h
}

// ready simulates READY + GUILD_CREATE for the bound guild and waits for
// provisioning (and whatever it schedules).
func (h *harness) ready(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	h.b.onReady(ctx, &discordgo.Ready{User: &discordgo.User{ID: botID}, Guilds: []*discordgo.Guild{{ID: guildID}}})
	h.b.onGuildCreate(ctx, &discordgo.Guild{ID: guildID})
	h.wait()
	_, ok := h.b.guild(wsID)
	require.True(t, ok, "guild provisioned")
}

func (h *harness) wait() { h.b.sched.Wait() }

func TestReadyReportsMissingGuildWithInvite(t *testing.T) {
	h := newHarness(t)
	h.b.onReady(context.Background(), &discordgo.Ready{User: &discordgo.User{ID: botID}})
	rec, ok, err := h.st.Health(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, rec.Bindings, 1)
	assert.False(t, rec.Bindings[0].BotMember)
	assert.Contains(t, rec.Bindings[0].InviteURL, "guild_id="+guildID)
}

func TestGuildCreateProvisionsBoundGuildOnly(t *testing.T) {
	h := newHarness(t)
	h.b.onGuildCreate(context.Background(), &discordgo.Guild{ID: "999999999999999999"})
	h.wait()
	assert.Empty(t, h.api.Calls(), "unbound guild ignored")

	h.ready(t)
	rec, _, err := h.st.Health(context.Background())
	require.NoError(t, err)
	assert.True(t, rec.Bindings[0].Provisioned)
	g, ok, err := h.st.Guild(context.Background(), wsID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, guildID, g.GuildID)
}

func (h *harness) addTask(id, title string) {
	h.host.AddTask(pluginsdk.Task{ID: id, WorkspaceID: wsID, Identifier: "KAN-" + id, Title: title, State: "IN_PROGRESS", CreatedAt: "2026-11-25T11:59:00Z"})
}

func (h *harness) event(t *testing.T, id, typ string, payload map[string]any) {
	t.Helper()
	h.b.HandleEvent(context.Background(), &pluginsdk.Event{EventID: id, EventType: typ, WorkspaceID: wsID, Payload: payload})
	h.wait()
}

func (h *harness) channel(t *testing.T, taskID string) store.Channel {
	t.Helper()
	rec, ok, err := h.st.Channel(context.Background(), taskID)
	require.NoError(t, err)
	require.True(t, ok, "channel record for %s", taskID)
	return rec
}

func TestTaskEventsDriveTheChannelLifecycle(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	h.addTask("1", "Fix login")
	h.api.ResetCalls()

	h.event(t, "e1", "task.created", map[string]any{"task_id": "1", "workspace_id": wsID})
	rec := h.channel(t, "1")
	assert.Equal(t, store.StatusActive, rec.Status)
	assert.Equal(t, "kan-1-fix-login", h.api.Channels[rec.ChannelID].Name)
	ix, ok, err := h.st.ChannelIndex(context.Background(), rec.ChannelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "1", ix.TaskID)

	h.event(t, "e1", "task.created", map[string]any{"task_id": "1"})
	h.event(t, "e2", "task.updated", map[string]any{"task_id": "1"})
	assert.Len(t, h.api.CallsTo("GuildChannelCreate"), 1, "duplicate and no-op events create nothing")

	archived := "2026-11-25T12:00:00Z"
	h.host.Tasks["1"].ArchivedAt = &archived
	h.event(t, "e3", "task.updated", map[string]any{"task_id": "1", "archived_at": archived})
	assert.Equal(t, store.StatusArchived, h.channel(t, "1").Status)
}

func TestDiscordChannelDeleteMarksRecordDeleted(t *testing.T) {
	h := newHarness(t)
	h.ready(t)
	h.addTask("1", "Fix login")
	h.event(t, "e1", "task.created", map[string]any{"task_id": "1"})
	rec := h.channel(t, "1")

	h.b.onChannelDelete(context.Background(), &discordgo.Channel{ID: rec.ChannelID})
	h.wait()
	assert.Equal(t, store.StatusDeleted, h.channel(t, "1").Status)
	_, ok, _ := h.st.ChannelIndex(context.Background(), rec.ChannelID)
	assert.False(t, ok)
}

func (h *harness) texts(channelID string) []string {
	var out []string
	for _, m := range h.api.SentMessages(channelID) {
		if m.Content != "" {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestTurnCompletedMirrorsAgentOutputWithoutEchoes(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	h.host.Sessions["1"] = []pluginsdk.Session{{ID: "s1", TaskID: "1", State: "WAITING_FOR_INPUT", AgentDisplayName: "Claude"}}
	h.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "1", SessionID: "s1", AuthorType: "user", Type: "message", Content: "alice (Discord): go", Source: "plugin:kandev-plugin-discord", CreatedAt: "2026-11-25T12:00:01Z"})
	h.host.AddMessage(pluginsdk.Message{ID: "m2", TaskID: "1", SessionID: "s1", AuthorType: "agent", Type: "thinking", Content: "secret plan", CreatedAt: "2026-11-25T12:00:02Z"})
	h.host.AddMessage(pluginsdk.Message{ID: "m3", TaskID: "1", SessionID: "s1", AuthorType: "agent", Type: "message", Content: "Fixed it.", CreatedAt: "2026-11-25T12:00:03Z"})

	h.event(t, "e1", "turn.completed", map[string]any{"task_id": "1", "session_id": "s1"})
	assert.Equal(t, []string{"Fixed it."}, h.texts(rec.ChannelID))
	assert.Equal(t, "m3", h.channel(t, "1").OutboundCursor.IDs[0])

	h.event(t, "e2", "task_session.state_changed", map[string]any{"session_id": "s1"})
	assert.Equal(t, []string{"Fixed it."}, h.texts(rec.ChannelID), "session-only event maps to the task; nothing new")
}

func TestOperatorChannelMessageReachesTheTaskOnce(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Fix login")
	h.ready(t)
	rec := h.channel(t, "1")
	g, _ := h.b.guild(wsID)
	ctx := context.Background()
	msg := &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: h.api.NewID(), ChannelID: rec.ChannelID, GuildID: guildID, Content: "ship it",
		Author: &discordgo.User{ID: "500000000000000001", Username: "alice"},
		Member: &discordgo.Member{Roles: []string{g.OperatorRoleID}},
	}}
	h.b.onMessageCreate(ctx, msg)
	h.b.onMessageCreate(ctx, msg)
	h.wait()
	assert.Equal(t, []kandev.SentMessage{{TaskID: "1", Text: "alice (Discord): ship it"}}, h.host.Sent)
	assert.Equal(t, msg.ID, h.channel(t, "1").InboundCursor)

	other := *msg.Message
	other.ID, other.GuildID = h.api.NewID(), "999999999999999999"
	h.b.onMessageCreate(ctx, &discordgo.MessageCreate{Message: &other})
	h.wait()
	assert.Len(t, h.host.Sent, 1, "unbound guild ignored")
}
