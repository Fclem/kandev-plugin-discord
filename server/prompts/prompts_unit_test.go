package prompts

import (
	"context"
	"errors"
	"strings"
	"testing"

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

func TestParsePermissionButtonRejectsMalformedIDs(t *testing.T) {
	id, idx, ok := ParsePermissionButton(PermissionButtonID("abc:def", 4))
	require.True(t, ok)
	assert.Equal(t, "abc:def", id, "interaction IDs may contain colons")
	assert.Equal(t, 4, idx)

	for _, bad := range []string{"kd:step:s:k:n", "kd:perm:", "kd:perm::1", "kd:perm:i1", "kd:perm:i1:x"} {
		_, _, ok := ParsePermissionButton(bad)
		assert.False(t, ok, bad)
	}
}

func TestParseStepButton(t *testing.T) {
	s, k, n, ok := ParseStepButton(StepButtonID("s1", StepKey("a", "b"), "nonce"))
	require.True(t, ok)
	assert.Equal(t, "s1", s)
	assert.Equal(t, StepKey("a", "b"), k)
	assert.Equal(t, "nonce", n)
	assert.Len(t, k, 12)
	assert.NotEqual(t, StepKey("a", "b"), StepKey("a", "c"))

	for _, bad := range []string{"kd:perm:i:1", "kd:step:s1:k", "kd:step:s1:k:n:x", "kd:step::k:n", "kd:step:s1::n", "kd:step:s1:k:"} {
		_, _, _, ok := ParseStepButton(bad)
		assert.False(t, ok, bad)
	}
}

func TestResolvedText(t *testing.T) {
	alice := &pluginsdk.InteractionActor{DisplayName: "Alice"}
	cases := []struct {
		name  string
		it    *pluginsdk.Interaction
		actor *pluginsdk.InteractionActor
		want  string
	}{
		{"unknown interaction", nil, nil, "✅ Resolved elsewhere."},
		{"denied with actor", &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusRejected}, alice, "✅ Denied by Alice."},
		{"answered by resolver", &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusAnswered, ResolvedBy: &pluginsdk.InteractionActor{DisplayName: "Bob", Via: "kandev"}}, nil, "✅ Answered by Bob via kandev."},
		{"explicit actor wins", &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusApproved, ResolvedBy: &pluginsdk.InteractionActor{DisplayName: "Bob"}}, alice, "✅ Approved by Alice."},
		{"cancelled", &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusCancelled}, alice, "⏹️ No longer pending."},
		{"expired", &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusExpired}, nil, "⏹️ No longer pending."},
		{"actor without name", &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusApproved}, &pluginsdk.InteractionActor{Via: "discord"}, "✅ Resolved elsewhere."},
		{"actor with no interaction", nil, alice, "✅ Resolved by Alice."},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ResolvedText(tc.it, tc.actor), tc.name)
	}
}

func TestPermissionPromptShowsContextAndNeutralButtons(t *testing.T) {
	f := newFixture(t)
	it := permission("i1", pluginsdk.InteractionOption{OptionID: "o", Label: strings.Repeat("L", 100), Kind: "other"})
	it.Context = "  in /repo  "
	f.host.Interactions["i1"] = it
	f.sync(t)
	m := f.api.SentMessages(f.rec.ChannelID)[0]
	assert.Equal(t, "🔐 **Permission request:** Run `rm -rf build`?\nin /repo", m.Content)
	b := buttons(m)
	require.Len(t, b, 1)
	assert.Equal(t, discordgo.SecondaryButton, b[0].Style)
	assert.Len(t, []rune(b[0].Label), maxButtonLabel, "labels fit Discord's limit")
	assert.True(t, strings.HasSuffix(b[0].Label, "…"))
}

func TestClarificationWithoutURLUsesQuestionTitles(t *testing.T) {
	f := newFixture(t)
	f.p.TaskURL = func(string) string { return "" }
	f.host.Interactions["q1"] = &pluginsdk.Interaction{ID: "q1", Kind: pluginsdk.InteractionKindClarification, TaskID: "t1",
		Status: pluginsdk.InteractionStatusPending, Questions: []pluginsdk.InteractionQuestion{{Title: "Database"}, {Prompt: "  "}}}
	f.sync(t)
	m := f.api.SentMessages(f.rec.ChannelID)[0]
	assert.Equal(t, "❓ **The agent asked a question.** Answer it in Kandev\n> Database", m.Content)
}

func TestOverflowWithoutURLStillCapsButtons(t *testing.T) {
	f := newFixture(t)
	f.p.TaskURL = func(string) string { return "" }
	var opts []pluginsdk.InteractionOption
	for range 26 {
		opts = append(opts, pluginsdk.InteractionOption{Label: "x"})
	}
	f.host.Interactions["i1"] = permission("i1", opts...)
	f.sync(t)
	m := f.api.SentMessages(f.rec.ChannelID)[0]
	assert.Len(t, buttons(m), maxButtons)
	assert.NotContains(t, m.Content, "More options")
}

func TestPromptPostFailureIsReturnedAndNothingRecorded(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["i1"] = permission("i1", allowReject...)
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 500})
	err := f.p.Sync(context.Background(), f.task, f.rec, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "posting prompt")
	stored, _ := f.st.Prompts(context.Background(), "t1")
	assert.Empty(t, stored.Items)
}

func TestListingFailureStopsTheSync(t *testing.T) {
	f := newFixture(t)
	f.host.PendingErr = status.Error(codes.Unavailable, "down")
	err := f.p.Sync(context.Background(), f.task, f.rec, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing pending interactions")
	assert.Empty(t, f.api.Calls())
}

func TestEndedInteractionLookups(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["late"] = permission("late", allowReject...)
	f.host.Interactions["gone"] = permission("gone", allowReject...)
	f.sync(t)
	sent := f.api.SentMessages(f.rec.ChannelID)
	require.Len(t, sent, 2)

	// "late" is still pending but missing from the listing (eventual
	// consistency); "gone" no longer exists in Kandev.
	late := f.host.Interactions["late"]
	delete(f.host.Interactions, "gone")
	late.TaskID = "elsewhere" // excluded from the task's pending list, still pending by ID
	f.sync(t)
	stored, _ := f.st.Prompts(context.Background(), "t1")
	require.Len(t, stored.Items, 1)
	assert.Equal(t, "late", stored.Items[0].InteractionID)
	var goneMsg *discordgo.Message
	for _, m := range f.api.SentMessages(f.rec.ChannelID) {
		if m.ID != stored.Items[0].MessageID {
			goneMsg = m
		}
	}
	require.NotNil(t, goneMsg)
	assert.Contains(t, goneMsg.Content, "✅ Resolved elsewhere.")
	assert.Empty(t, goneMsg.Components)
}

func TestTransientLookupFailureKeepsThePrompt(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["i1"] = permission("i1", allowReject...)
	f.sync(t)
	f.host.Interactions["i1"].TaskID = "elsewhere"
	f.host.Interactions["i1"].Status = pluginsdk.InteractionStatusApproved
	// GetInteraction fails with a non-NotFound error: keep the prompt.
	h := &failingGet{Fake: f.host, err: status.Error(codes.Unavailable, "down")}
	f.p.Host = h
	f.sync(t)
	stored, _ := f.st.Prompts(context.Background(), "t1")
	assert.Len(t, stored.Items, 1)
	assert.NotEmpty(t, f.api.SentMessages(f.rec.ChannelID)[0].Components, "buttons stay until the outcome is known")
}

func TestResolveFailureKeepsThePromptForRetry(t *testing.T) {
	f := newFixture(t)
	f.host.Interactions["i1"] = permission("i1", allowReject...)
	f.sync(t)
	f.host.Interactions["i1"].Status = pluginsdk.InteractionStatusApproved
	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 503})
	f.sync(t)
	stored, _ := f.st.Prompts(context.Background(), "t1")
	assert.Len(t, stored.Items, 1)

	f.sync(t)
	stored, _ = f.st.Prompts(context.Background(), "t1")
	assert.Empty(t, stored.Items, "the next sync resolves it")
}

func TestResolveTreatsAGoneMessageAsResolved(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.p.Resolve(context.Background(), f.rec.ChannelID, "999", "done"))
	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 403, Code: discordapi.CodeMissingAccess})
	err := f.p.Resolve(context.Background(), f.rec.ChannelID, "999", "done")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "updating prompt 999")
}

func TestResolveKeepsTheOriginalText(t *testing.T) {
	f := newFixture(t)
	m := f.api.AddMessage(&discordgo.Message{ChannelID: f.rec.ChannelID, Content: "question?"})
	require.NoError(t, f.p.Resolve(context.Background(), f.rec.ChannelID, m.ID, "answered"))
	assert.Equal(t, "question?\n\nanswered", f.api.Message(f.rec.ChannelID, m.ID).Content)

	// Non-numeric IDs cannot be stepped back; the edit still happens.
	assert.Equal(t, "abc", prevID("abc"))
	assert.Equal(t, "0", prevID("0"))
	assert.Equal(t, "41", prevID("42"))
}

func TestCloseResolvesEveryPromptAndTheStepPrompt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.Interactions["i1"] = permission("i1", allowReject...)
	f.host.Interactions["i2"] = permission("i2", allowReject...)
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, Awaiting: true, FromStepID: "a", FromStepName: "A", ToStepID: "b"}, nil
	}
	f.sync(t, pluginsdk.Session{ID: "s1", State: "WAITING_FOR_INPUT"})
	require.Len(t, f.api.SentMessages(f.rec.ChannelID), 3)

	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 500})
	err := f.p.Close(ctx, "t1", f.rec.ChannelID)
	require.Error(t, err, "one edit failed")
	stored, _ := f.st.Prompts(ctx, "t1")
	assert.Len(t, stored.Items, 1, "the failed prompt stays recorded")
	_, posted, _ := f.st.StepApproval(ctx, "t1")
	assert.False(t, posted)

	require.NoError(t, f.p.Close(ctx, "t1", f.rec.ChannelID))
	stored, _ = f.st.Prompts(ctx, "t1")
	assert.Empty(t, stored.Items)
	for _, m := range f.api.SentMessages(f.rec.ChannelID) {
		assert.Contains(t, m.Content, closedText)
		assert.Empty(t, m.Components)
	}
}

func TestCloseKeepsAStepPromptWhoseEditFailed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, Awaiting: true, FromStepID: "a", ToStepID: "b"}, nil
	}
	f.sync(t, pluginsdk.Session{ID: "s1", State: "WAITING_FOR_INPUT"})
	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 500})
	require.Error(t, f.p.Close(ctx, "t1", f.rec.ChannelID))
	_, posted, _ := f.st.StepApproval(ctx, "t1")
	assert.True(t, posted)
	assert.NoError(t, f.p.Close(ctx, "t1", f.rec.ChannelID))
}

func TestStepApprovalReadFailureIsReturned(t *testing.T) {
	f := newFixture(t)
	f.host.StepFn = func(pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{}, status.Error(codes.Internal, "boom")
	}
	err := f.p.Sync(context.Background(), f.task, f.rec, []pluginsdk.Session{{ID: "s1", State: "WAITING_FOR_INPUT"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading step approval")
}

func TestDeniedOrGoneSessionsAreSkipped(t *testing.T) {
	f := newFixture(t)
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		switch q.SessionID {
		case "denied":
			return pluginsdk.StepApproval{}, status.Error(codes.PermissionDenied, "no grant")
		case "gone":
			return pluginsdk.StepApproval{}, status.Error(codes.NotFound, "gone")
		}
		return pluginsdk.StepApproval{Awaiting: true, FromStepID: "a", FromStepName: "A", ToStepID: "b"}, nil
	}
	f.sync(t, pluginsdk.Session{ID: "denied"}, pluginsdk.Session{ID: "gone"}, pluginsdk.Session{ID: "ok"})
	step, posted, err := f.st.StepApproval(context.Background(), "t1")
	require.NoError(t, err)
	require.True(t, posted)
	assert.Equal(t, "ok", step.SessionID)
	assert.Equal(t, "⏸️ Step **A** awaits approval. Approve to move on, or post a message to request changes.",
		f.api.SentMessages(f.rec.ChannelID)[0].Content, "no target step name, no arrow")
}

func TestStepPromptPostFailureIsReturned(t *testing.T) {
	f := newFixture(t)
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{Awaiting: true, FromStepID: "a", ToStepID: "b"}, nil
	}
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 500})
	err := f.p.Sync(context.Background(), f.task, f.rec, []pluginsdk.Session{{ID: "s1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "posting step approval")
	_, posted, _ := f.st.StepApproval(context.Background(), "t1")
	assert.False(t, posted)
}

func TestStepRefreshOfAGonePromptPostsANewOne(t *testing.T) {
	f := newFixture(t)
	version := "v1"
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{Awaiting: true, FromStepID: "a", ToStepID: "b", ResourceVersion: version}, nil
	}
	idle := pluginsdk.Session{ID: "s1"}
	f.sync(t, idle)
	first, _, _ := f.st.StepApproval(context.Background(), "t1")

	version = "v2"
	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 404, Code: discordapi.CodeUnknownMessage})
	f.sync(t, idle)
	now, posted, _ := f.st.StepApproval(context.Background(), "t1")
	require.True(t, posted)
	assert.NotEqual(t, first.MessageID, now.MessageID, "a deleted prompt is replaced")
	assert.Equal(t, "v2", now.ResourceVersion)
	assert.Len(t, f.api.SentMessages(f.rec.ChannelID), 2)
}

func TestStepRefreshFailureIsReturned(t *testing.T) {
	f := newFixture(t)
	version := "v1"
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{Awaiting: true, FromStepID: "a", ToStepID: "b", ResourceVersion: version}, nil
	}
	idle := pluginsdk.Session{ID: "s1"}
	f.sync(t, idle)
	first, _, _ := f.st.StepApproval(context.Background(), "t1")

	version = "v2"
	f.api.FailNext("MessageEdit", &discordapi.Error{Status: 500})
	err := f.p.Sync(context.Background(), f.task, f.rec, []pluginsdk.Session{idle})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refreshing step approval")
	now, _, _ := f.st.StepApproval(context.Background(), "t1")
	assert.Equal(t, first, now, "the record still describes the live button")
}

func TestStoreFailuresSurface(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("state down")
	f.p.Store = store.New(&brokenState{err: boom})
	err := f.p.Sync(context.Background(), f.task, f.rec, nil)
	assert.ErrorIs(t, err, boom)
	assert.ErrorIs(t, f.p.Close(context.Background(), "t1", f.rec.ChannelID), boom)
}

func TestTruncateCountsRunes(t *testing.T) {
	assert.Equal(t, "héllo", truncate("héllo", 5))
	assert.Equal(t, "hé…", truncate("héllo", 3))
}

// failingGet makes GetInteraction fail with err.
type failingGet struct {
	*kandev.Fake
	err error
}

func (f *failingGet) GetInteraction(context.Context, string) (*pluginsdk.Interaction, error) {
	return nil, f.err
}

// brokenState fails every Host state call.
type brokenState struct{ err error }

func (b *brokenState) GetState(context.Context, string, string, string) (map[string]any, bool, error) {
	return nil, false, b.err
}
func (b *brokenState) SetState(context.Context, string, string, string, map[string]any) error {
	return b.err
}
func (b *brokenState) DeleteState(context.Context, string, string, string) error { return b.err }
func (b *brokenState) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	return nil, b.err
}
