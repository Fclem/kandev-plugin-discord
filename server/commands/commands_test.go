package commands

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

const (
	guildID   = "223456789012345678"
	wsID      = "ws-1"
	channelID = "400000000000000001"
	opRole    = "300000000000000001"
	opUser    = "500000000000000001"
	guestUser = "500000000000000002"
)

type fixture struct {
	r     *Router
	api   *discordapi.Fake
	host  *kandev.Fake
	st    *store.Store
	seq   int
	syncs []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{api: discordapi.NewFake(), host: kandev.NewFake(), st: store.New(store.NewMemoryHost())}
	cfg := config.Config{ApplicationID: "123456789012345678", KandevBaseURL: "https://kandev.example.com",
		Bindings: []config.Binding{{WorkspaceID: wsID, GuildID: guildID}}}
	guild := store.Guild{GuildID: guildID, OperatorRoleID: opRole}
	require.NoError(t, f.st.PutChannel(context.Background(), store.Channel{TaskID: "t1", WorkspaceID: wsID, GuildID: guildID, ChannelID: channelID, Status: store.StatusActive}, ""))
	f.host.AddTask(pluginsdk.Task{ID: "t1", WorkspaceID: wsID, Identifier: "KAN-1", Title: "Fix login", State: "IN_PROGRESS", WorkflowID: "wf", WorkflowStepID: "st"})
	f.r = &Router{Env{
		API: f.api, Host: f.host, Config: cfg, Store: f.st,
		Guild: func(id string) (store.Guild, config.Binding, bool) {
			return guild, cfg.Bindings[0], id == guildID
		},
		QueueCount: func(string) int { return 2 },
		StepName:   func(context.Context, *pluginsdk.Task) string { return "Review" },
		OnTask:     func(_ string, fn func(context.Context)) { fn(context.Background()) },
		Refresh:    func(id string) { f.syncs = append(f.syncs, id) },
	}}
	return f
}

func (f *fixture) interaction(user string, typ discordgo.InteractionType, data discordgo.InteractionData) *discordgo.Interaction {
	f.seq++
	roles := []string{}
	if user == opUser {
		roles = []string{opRole}
	}
	return &discordgo.Interaction{
		ID: "ix" + string(rune('a'+f.seq)), Type: typ, GuildID: guildID, ChannelID: channelID, Data: data,
		Member: &discordgo.Member{Roles: roles, User: &discordgo.User{ID: user, Username: user}},
	}
}

func (f *fixture) command(user, name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return f.interaction(user, discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: name, Options: opts})
}

func opt(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value}
}

func (f *fixture) run(i *discordgo.Interaction) string {
	f.r.Handle(context.Background(), i)
	return f.api.LastReply(i.ID)
}

func TestCommandsAreDeferredEphemerallyFirst(t *testing.T) {
	f := newFixture(t)
	i := f.command(guestUser, CmdStatus)
	f.run(i)
	calls := f.api.Calls()
	require.NotEmpty(t, calls)
	assert.Equal(t, "InteractionRespond", calls[0].Method)
	resp := f.api.Interactions[i.ID][0].(*discordgo.InteractionResponse)
	assert.Equal(t, discordgo.InteractionResponseDeferredChannelMessageWithSource, resp.Type)
	assert.Equal(t, discordgo.MessageFlagsEphemeral, resp.Data.Flags)
}

func TestCommandsOutsideTaskChannels(t *testing.T) {
	f := newFixture(t)
	i := f.command(opUser, CmdStatus)
	i.ChannelID = "999"
	assert.Equal(t, "Commands work only in Kandev task channels.", f.run(i))

	unbound := f.command(opUser, CmdStatus)
	unbound.GuildID = "999999999999999999"
	assert.Contains(t, f.run(unbound), "not bound")
}

func TestStatusForAnyone(t *testing.T) {
	f := newFixture(t)
	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s1", State: "RUNNING", AgentDisplayName: "Claude"}}
	f.host.Interactions["p1"] = &pluginsdk.Interaction{ID: "p1", TaskID: "t1", Status: pluginsdk.InteractionStatusPending}
	got := f.run(f.command(guestUser, CmdStatus))
	for _, want := range []string{"**KAN-1 · Fix login**", "State: in progress · Step: Review", "• Claude — running", "Queued messages: 2 · Pending requests: 1", "https://kandev.example.com/t/t1"} {
		assert.Contains(t, got, want)
	}
}

func TestStatusFitsDiscordsLimitWithManySessions(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("A", 300)
	for i := range 60 {
		f.host.Sessions["t1"] = append(f.host.Sessions["t1"], pluginsdk.Session{ID: fmt.Sprint("s", i), State: "WAITING_FOR_INPUT", AgentDisplayName: long})
	}
	got := f.run(f.command(guestUser, CmdStatus))
	assert.LessOrEqual(t, utf8.RuneCountInString(got), 2000)
	assert.Contains(t, got, "… and 50 more")
	assert.Contains(t, got, "https://kandev.example.com/t/t1", "the link survives")
	assert.LessOrEqual(t, utf8.RuneCountInString(clip(strings.Repeat("line\n", 1000), 2000)), 2000)
}

func running(id string) pluginsdk.ExactSessionObservation {
	return pluginsdk.ExactSessionObservation{Session: pluginsdk.Session{ID: id, State: "RUNNING", AgentDisplayName: "Claude"}, ExecutionID: "ex-" + id, ResourceVersion: "v-" + id}
}

func TestStopTurnCancelsTheRunningTurnWithExactFences(t *testing.T) {
	f := newFixture(t)
	idle := running("s2")
	idle.Session.State = "WAITING_FOR_INPUT"
	f.host.Exact["t1"] = []pluginsdk.ExactSessionObservation{running("s1"), idle}
	i := f.command(opUser, CmdStop)
	got := f.run(i)
	assert.Equal(t, "⏹️ Cancelled the current turn of Claude; the session stays open.", got)
	require.Len(t, f.host.Cancels, 1)
	cmd := f.host.Cancels[0]
	assert.Equal(t, "s1", cmd.SessionID)
	assert.Equal(t, "ex-s1", cmd.ExpectedExecutionID)
	assert.Equal(t, "v-s1", cmd.ExpectedSessionResourceVersion)
	assert.Equal(t, uint64(7), cmd.ApprovalRevision)
	assert.Equal(t, "digest", cmd.ManifestDigest)
	assert.Equal(t, "", cmd.ManagementInstanceKey)
	assert.Equal(t, int64(0), cmd.ExpectedClaimGeneration)
	assert.Equal(t, "discord:stop:turn:"+i.ID+":s1", cmd.IdempotencyKey)
	assert.Empty(t, f.host.Stops)
}

func TestStopTaskStopsEverySessionWithAnExecution(t *testing.T) {
	f := newFixture(t)
	idle := running("s2")
	idle.Session.State = "WAITING_FOR_INPUT"
	f.host.Exact["t1"] = []pluginsdk.ExactSessionObservation{running("s1"), idle}
	f.run(f.command(opUser, CmdStop, opt(OptScope, ScopeTask)))
	assert.Len(t, f.host.Stops, 2)
	assert.Empty(t, f.host.Cancels)
}

func TestStopOutcomesAndRefusals(t *testing.T) {
	f := newFixture(t)
	assert.Equal(t, "Only members with the operator role can do that.", f.run(f.command(guestUser, CmdStop)))
	assert.Equal(t, "Nothing is running.", f.run(f.command(opUser, CmdStop)), "no running session")

	f.host.Exact["t1"] = []pluginsdk.ExactSessionObservation{running("s1")}
	cases := map[pluginsdk.CommandStatus]string{
		pluginsdk.CommandNoChange:    "Nothing is running.",
		pluginsdk.CommandConflict:    "The session changed (or the task is managed elsewhere); run /stop again.",
		pluginsdk.CommandDenied:      msgExecutionGrant,
		pluginsdk.CommandUnsupported: msgUnsupported,
	}
	for st, want := range cases {
		f.host.CancelFn = func(pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
			return &pluginsdk.CommandResult{Status: st}, false, nil
		}
		assert.Equal(t, want, f.run(f.command(opUser, CmdStop)), st)
	}
	f.host.ExactErr = status.Error(codes.PermissionDenied, "missing host.v2.read:sessions")
	assert.Equal(t, msgExecutionGrant, f.run(f.command(opUser, CmdStop)))
}
