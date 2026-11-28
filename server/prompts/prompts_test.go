package prompts

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

type fixture struct {
	api  *discordapi.Fake
	host *kandev.Fake
	st   *store.Store
	p    *Poster
	task *pluginsdk.Task
	rec  store.Channel
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{api: discordapi.NewFake(), host: kandev.NewFake(), st: store.New(store.NewMemoryHost())}
	ch := f.api.AddChannel(&discordgo.Channel{GuildID: "g"})
	f.task = f.host.AddTask(pluginsdk.Task{ID: "t1", WorkspaceID: "ws", Title: "One"})
	f.rec = store.Channel{TaskID: "t1", ChannelID: ch.ID, Status: store.StatusActive}
	f.p = &Poster{API: f.api, Host: f.host, Store: f.st, TaskURL: func(id string) string { return "https://k/t/" + id }, Now: time.Now}
	return f
}

func (f *fixture) sync(t *testing.T, sessions ...pluginsdk.Session) {
	t.Helper()
	require.NoError(t, f.p.Sync(context.Background(), f.task, f.rec, sessions))
}

func permission(id string, options ...pluginsdk.InteractionOption) *pluginsdk.Interaction {
	return &pluginsdk.Interaction{ID: id, Kind: pluginsdk.InteractionKindPermission, TaskID: "t1", SessionID: "s1",
		Status: pluginsdk.InteractionStatusPending, Title: "Run `rm -rf build`?", Options: options}
}

var allowReject = []pluginsdk.InteractionOption{
	{OptionID: "o1", Label: "Allow once", Kind: "allow_once"},
	{OptionID: "o2", Label: "Always allow", Kind: "allow_always"},
	{OptionID: "o3", Label: "Reject", Kind: "reject_once"},
}

func buttons(m *discordgo.Message) []discordgo.Button {
	var out []discordgo.Button
	for _, row := range m.Components {
		for _, c := range row.(discordgo.ActionsRow).Components {
			out = append(out, c.(discordgo.Button))
		}
	}
	return out
}

func TestPermissionPromptIsPostedOnceWithAButtonPerOption(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["i1"] = permission("i1", allowReject...)
	f.sync(t)
	f.sync(t)
	sent := f.api.SentMessages(f.rec.ChannelID)
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Content, "Permission request:")
	b := buttons(sent[0])
	require.Len(t, b, 3)
	assert.Equal(t, discordgo.SuccessButton, b[0].Style)
	assert.Equal(t, discordgo.DangerButton, b[2].Style)
	id, idx, ok := ParsePermissionButton(b[2].CustomID)
	assert.True(t, ok)
	assert.Equal(t, "i1", id)
	assert.Equal(t, 2, idx)
	stored, err := f.st.Prompts(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, sent[0].ID, stored.Items[0].MessageID)
}

func TestOptionOverflowLinksToKandev(t *testing.T) {
	f := newFixture(t)
	var opts []pluginsdk.InteractionOption
	for i := range 30 {
		opts = append(opts, pluginsdk.InteractionOption{OptionID: fmt.Sprint(i), Label: fmt.Sprint("Option ", i)})
	}
	f.host.Interactions["i1"] = permission("i1", opts...)
	f.sync(t)
	sent := f.api.SentMessages(f.rec.ChannelID)[0]
	assert.Len(t, buttons(sent), 25)
	assert.Len(t, sent.Components, 5)
	assert.Contains(t, sent.Content, "More options in Kandev: https://k/t/t1")
}

func TestResolvedPromptShowsTheActorAndLosesItsButtons(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["i1"] = permission("i1", allowReject...)
	f.sync(t)
	f.host.Interactions["i1"].Status = pluginsdk.InteractionStatusApproved
	f.host.Interactions["i1"].ResolvedBy = &pluginsdk.InteractionActor{DisplayName: "Alice", Via: "discord"}
	f.sync(t)
	m := f.api.SentMessages(f.rec.ChannelID)[0]
	assert.Contains(t, m.Content, "Run `rm -rf build`?")
	assert.Contains(t, m.Content, "✅ Approved by Alice via discord.")
	assert.Empty(t, m.Components)
	stored, _ := f.st.Prompts(context.Background(), "t1")
	assert.Empty(t, stored.Items)
}

func TestClarificationGetsANoticeWithALink(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["q1"] = &pluginsdk.Interaction{ID: "q1", Kind: pluginsdk.InteractionKindClarification, TaskID: "t1",
		Status: pluginsdk.InteractionStatusPending, Questions: []pluginsdk.InteractionQuestion{{Prompt: "Which DB?"}}}
	f.sync(t)
	m := f.api.SentMessages(f.rec.ChannelID)[0]
	assert.Contains(t, m.Content, "Answer it in Kandev: https://k/t/t1")
	assert.Contains(t, m.Content, "> Which DB?")
	assert.Empty(t, m.Components)
}

func TestStepApprovalPromptFollowsGetStepApproval(t *testing.T) {
	f := newFixture(t)
	awaiting := true
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, TaskID: "t1", Awaiting: awaiting,
			FromStepID: "review", FromStepName: "Review", ToStepID: "done", ToStepName: "Done", ResourceVersion: "v1"}, nil
	}
	idle := pluginsdk.Session{ID: "s1", State: "WAITING_FOR_INPUT"}
	f.sync(t, idle)
	f.sync(t, idle)
	sent := f.api.SentMessages(f.rec.ChannelID)
	require.Len(t, sent, 1)
	assert.Equal(t, "⏸️ Step **Review** awaits approval → **Done**. Approve to move on, or post a message to request changes.", sent[0].Content)
	step, posted, err := f.st.StepApproval(context.Background(), "t1")
	require.NoError(t, err)
	require.True(t, posted)
	assert.NotEmpty(t, step.Nonce)
	assert.Equal(t, StepButtonID("s1", StepKey("review", "done"), step.Nonce), buttons(sent[0])[0].CustomID)
	assert.LessOrEqual(t, len(StepButtonID("e7d1e2c4-0b8f-4a57-9f3e-6d1a2b3c4d5e", StepKey("review", "done"), step.Nonce)), 100)

	awaiting = false
	f.sync(t, idle)
	assert.Contains(t, f.api.SentMessages(f.rec.ChannelID)[0].Content, "No longer pending.")
	_, posted, err = f.st.StepApproval(context.Background(), "t1")
	require.NoError(t, err)
	assert.False(t, posted)
}

func TestUnresolvedStepPromptIsRetriedBeforeANewOne(t *testing.T) {
	f := newFixture(t)
	to := "done"
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, TaskID: "t1", Awaiting: true,
			FromStepID: "review", FromStepName: "Review", ToStepID: to, ToStepName: to}, nil
	}
	idle := pluginsdk.Session{ID: "s1", State: "WAITING_FOR_INPUT"}
	f.sync(t, idle)
	first := f.api.SentMessages(f.rec.ChannelID)[0]

	to = "deploy"
	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 503})
	require.Error(t, f.p.Sync(context.Background(), f.task, f.rec, []pluginsdk.Session{idle}))
	step, posted, _ := f.st.StepApproval(context.Background(), "t1")
	require.True(t, posted, "kept until its buttons are gone")
	assert.Equal(t, first.ID, step.MessageID)
	assert.NotEmpty(t, buttons(f.api.Message(f.rec.ChannelID, first.ID)), "still live in Discord")

	f.sync(t, idle)
	assert.Empty(t, f.api.Message(f.rec.ChannelID, first.ID).Components, "the retry removed the old buttons")
	sent := f.api.SentMessages(f.rec.ChannelID)
	assert.Contains(t, sent[len(sent)-1].Content, "**deploy**")
}

func TestStepPromptButtonRotatesWhenTheApprovalStateChanges(t *testing.T) {
	f := newFixture(t)
	version := "v1"
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, TaskID: "t1", Awaiting: true,
			FromStepID: "review", FromStepName: "Review", ToStepID: "done", ToStepName: "Done", ResourceVersion: version}, nil
	}
	idle := pluginsdk.Session{ID: "s1", State: "WAITING_FOR_INPUT"}
	f.sync(t, idle)
	first, _, _ := f.st.StepApproval(context.Background(), "t1")

	version = "v2" // a later visit to the same transition
	f.sync(t, idle)
	sent := f.api.SentMessages(f.rec.ChannelID)
	require.Len(t, sent, 1, "no new message")
	now, _, _ := f.st.StepApproval(context.Background(), "t1")
	assert.Equal(t, "v2", now.ResourceVersion)
	assert.NotEqual(t, first.Nonce, now.Nonce)
	assert.Equal(t, StepButtonID("s1", StepKey("review", "done"), now.Nonce), buttons(sent[0])[0].CustomID, "the old button is gone")
}

func TestOfficeStageAndRunningSessionsPostNoStepPrompt(t *testing.T) {
	f := newFixture(t)
	f.host.StepFn = func(pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{}, status.Error(codes.Unimplemented, "office approval stage")
	}
	f.sync(t, pluginsdk.Session{ID: "s1", State: "WAITING_FOR_INPUT"}, pluginsdk.Session{ID: "s2", State: "RUNNING"})
	assert.Empty(t, f.api.SentMessages(f.rec.ChannelID))
}
