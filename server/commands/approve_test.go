package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-discord/server/prompts"
	"kandev-plugin-discord/server/store"
)

var options = []pluginsdk.InteractionOption{
	{OptionID: "o-allow", Label: "Allow once", Kind: "allow_once"},
	{OptionID: "o-always", Label: "Always allow", Kind: "allow_always"},
	{OptionID: "o-reject", Label: "Reject", Kind: "reject_once"},
}

func (f *fixture) pendingPermission(t *testing.T, id string) string {
	t.Helper()
	f.host.Interactions[id] = &pluginsdk.Interaction{ID: id, Kind: pluginsdk.InteractionKindPermission, TaskID: "t1",
		Status: pluginsdk.InteractionStatusPending, Title: "Run tests?", Options: options}
	f.host.Revisions[id] = "rev-1"
	msg := f.api.AddMessage(&discordgo.Message{ChannelID: channelID, Content: "🔐 Run tests?", Author: f.api.BotUser})
	f.api.AddChannel(&discordgo.Channel{ID: channelID, GuildID: guildID})
	require.NoError(t, f.st.PutPrompts(context.Background(), "t1", store.Prompts{Items: []store.Prompt{{InteractionID: id, MessageID: msg.ID}}}))
	return msg.ID
}

func (f *fixture) button(user, customID string) *discordgo.Interaction {
	return f.interaction(user, discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: customID})
}

func TestButtonAnswersAsTheLinkedKandevUser(t *testing.T) {
	f := newFixture(t)
	promptID := f.pendingPermission(t, "i1")
	got := f.run(f.button(opUser, prompts.PermissionButtonID("i1", 1)))

	require.Len(t, f.host.Receipts, 1)
	r := f.host.Receipts[0]
	assert.Equal(t, pluginsdk.HumanResponseReceiptRequest{
		RequestID: r.RequestID, WorkspaceID: wsID, Provider: "discord", Subject: opUser,
		Kind: pluginsdk.HumanResponseKindPermission, TargetID: "i1", ExpectedResourceVersion: "rev-1",
		OptionID: "o-always", ApprovalRevision: 7, ManifestDigest: "digest",
	}, r)
	require.Len(t, f.host.Responses, 1)
	resp := f.host.Responses[0]
	assert.Equal(t, "receipt-i1", resp.HumanResponseReceiptID)
	assert.Equal(t, "o-always", resp.OptionID)
	assert.Equal(t, "rev-1", resp.ExpectedResourceVersion)
	assert.Equal(t, "✅ Always allow — recorded in Kandev as Alice via Discord.", got)

	prompt := f.api.Message(channelID, promptID)
	assert.Contains(t, prompt.Content, "✅ Always allow by Alice via discord.")
	assert.Empty(t, prompt.Components)
	assert.Contains(t, f.syncs, "t1", "the task is re-synced after the answer")
}

func TestUnlinkedOperatorGetsADeepLinkAndNothingChanges(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	f.host.ReceiptFn = func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandDenied, Reason: "external_identity_not_linked"}, pluginsdk.HumanResponseReceipt{}, nil
	}
	got := f.run(f.button(opUser, prompts.PermissionButtonID("i1", 0)))
	assert.Contains(t, got, "not linked")
	assert.Contains(t, got, "https://kandev.example.com/t/t1")
	assert.Contains(t, got, "/link <code>")
	assert.Empty(t, f.host.Responses)
}

func TestMissingGrantRepliesAndRequestsNoReceipt(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	f.host.Capabilities.ApprovalRevision = 0
	assert.Equal(t, msgApprovalGrant, f.run(f.command(opUser, CmdApprove)))
	assert.Empty(t, f.host.Receipts)

	f.host.Capabilities.ApprovalRevision = 7
	f.host.ReceiptFn = func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandDenied, Reason: "missing_capability_approval"}, pluginsdk.HumanResponseReceipt{}, nil
	}
	assert.Equal(t, msgApprovalGrant, f.run(f.command(opUser, CmdApprove)))
	assert.Empty(t, f.host.Responses)
}

func TestNonOperatorsCannotApprove(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	assert.Equal(t, "Only members with the operator role can do that.", f.run(f.button(guestUser, prompts.PermissionButtonID("i1", 0))))
	assert.Empty(t, f.host.Receipts)
}

func TestArchivedChannelButtonsAndCommandsChangeNothing(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	awaitingStep(f)
	require.NoError(t, f.st.PutChannel(context.Background(), store.Channel{TaskID: "t1", WorkspaceID: wsID, GuildID: guildID, ChannelID: channelID, Status: store.StatusArchived}, ""))
	for _, got := range []string{
		f.run(f.button(opUser, prompts.PermissionButtonID("i1", 0))),
		f.run(f.button(opUser, reviewButton(t, f))),
		f.run(f.command(opUser, CmdApprove)),
		f.run(f.command(opUser, CmdStop)),
	} {
		assert.Contains(t, got, "This channel is closed")
	}
	assert.Empty(t, f.host.Receipts)
	assert.Empty(t, f.host.Responses)
	assert.Empty(t, f.host.Approvals)
	assert.Contains(t, f.run(f.command(guestUser, CmdStatus)), "KAN-1", "/status still answers")
}

func TestApprovalDoesNotResolveThePromptOfALaterVisit(t *testing.T) {
	f := newFixture(t)
	awaitingStep(f)
	button := reviewButton(t, f)
	f.host.ApproveFn = func(pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error) {
		// Before the answer lands, the task revisits Review → Done and the
		// sync posts the new visit's prompt.
		require.NoError(t, f.st.PutStepApproval(context.Background(), "t1", store.StepApproval{
			SessionID: "s1", FromStepID: "review", ToStepID: "done", ResourceVersion: "sv-2", Nonce: "n2", MessageID: "m2",
		}))
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, pluginsdk.StepApprovalResult{}, nil
	}
	f.run(f.button(opUser, button))
	require.Len(t, f.host.Approvals, 1)
	step, posted, err := f.st.StepApproval(context.Background(), "t1")
	require.NoError(t, err)
	require.True(t, posted, "the new visit's prompt is kept")
	assert.Equal(t, "sv-2", step.ResourceVersion)
}

func TestAlreadyResolvedRequestMarksThePrompt(t *testing.T) {
	f := newFixture(t)
	promptID := f.pendingPermission(t, "i1")
	f.host.Interactions["i1"].Status = pluginsdk.InteractionStatusApproved
	f.host.Interactions["i1"].ResolvedBy = &pluginsdk.InteractionActor{DisplayName: "Bob", Via: "web"}
	assert.Equal(t, "That request was already resolved.", f.run(f.button(opUser, prompts.PermissionButtonID("i1", 0))))
	assert.Contains(t, f.api.Message(channelID, promptID).Content, "✅ Approved by Bob via web.")
	assert.Empty(t, f.host.Receipts)

	f2 := newFixture(t)
	f2.pendingPermission(t, "i2")
	f2.host.RespondFn = func(pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandConflict, Reason: "interaction_already_resolved"}, nil, nil
	}
	assert.Equal(t, "That request was already resolved.", f2.run(f2.button(opUser, prompts.PermissionButtonID("i2", 0))))
}

func TestApproveAndDenyPickTheirDefaultOptions(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	f.run(f.command(opUser, CmdDeny))
	require.Len(t, f.host.Responses, 1)
	assert.Equal(t, "o-reject", f.host.Responses[0].OptionID)

	f2 := newFixture(t)
	f2.pendingPermission(t, "i1")
	f2.run(f2.command(opUser, CmdApprove))
	assert.Equal(t, "o-allow", f2.host.Responses[0].OptionID)

	f3 := newFixture(t)
	f3.pendingPermission(t, "i1")
	f3.pendingPermission(t, "i2")
	assert.Contains(t, f3.run(f3.command(opUser, CmdApprove)), "choose one with the `request` option")
	f3.run(f3.command(opUser, CmdApprove, opt(OptRequest, "perm:i2")))
	assert.Equal(t, "i2", f3.host.Responses[0].InteractionID)
}

func awaitingStep(f *fixture) { awaitingStepAt(f, "sv-1") }

func awaitingStepAt(f *fixture, version string) {
	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s1", State: "WAITING_FOR_INPUT"}}
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, TaskID: "t1", Awaiting: true,
			FromStepID: "review", FromStepName: "Review", ToStepID: "done", ToStepName: "Done", ResourceVersion: version}, nil
	}
}

func TestApproveWithoutPermissionsApprovesTheStep(t *testing.T) {
	f := newFixture(t)
	awaitingStep(f)
	i := f.command(opUser, CmdApprove)
	got := f.run(i)
	require.Len(t, f.host.Receipts, 1)
	assert.Equal(t, pluginsdk.HumanResponseKindStepApproval, f.host.Receipts[0].Kind)
	assert.Equal(t, "s1", f.host.Receipts[0].TargetID)
	assert.Equal(t, "sv-1", f.host.Receipts[0].ExpectedResourceVersion)
	assert.Empty(t, f.host.Receipts[0].OptionID)
	require.Len(t, f.host.Approvals, 1)
	cmd := f.host.Approvals[0]
	assert.Equal(t, "receipt-s1", cmd.HumanResponseReceiptID)
	assert.Equal(t, "discord:approve:"+i.ID, cmd.IdempotencyKey)
	assert.Equal(t, "sv-1", cmd.ExpectedResourceVersion)
	assert.Equal(t, "✅ Step **Review** approved → **Done**, recorded in Kandev as Alice via Discord.", got)

	assert.Contains(t, f.run(f.command(opUser, CmdDeny)), "no permission request to deny")
	assert.Len(t, f.host.Approvals, 1, "/deny never applies to steps")
}

func TestStepApprovalOutcomes(t *testing.T) {
	f := newFixture(t)
	assert.Equal(t, msgNothingAwaiting, f.run(f.command(opUser, CmdApprove)))

	awaitingStep(f)
	f.host.ApproveFn = func(pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandConflict, Reason: "step_not_awaiting_approval"}, pluginsdk.StepApprovalResult{}, nil
	}
	assert.Equal(t, msgNothingAwaiting, f.run(f.button(opUser, reviewButton(t, f))))

	f.host.StepFn = func(pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{}, status.Error(codes.Unimplemented, "office approval stage")
	}
	got := f.run(f.button(opUser, reviewButton(t, f)))
	assert.Equal(t, "This is an Office approval stage; approve it in Kandev: https://kandev.example.com/t/t1", got)
}

// reviewButton records a posted Review → Done prompt and returns its button.
func reviewButton(t *testing.T, f *fixture) string {
	t.Helper()
	require.NoError(t, f.st.PutStepApproval(context.Background(), "t1", store.StepApproval{
		SessionID: "s1", FromStepID: "review", ToStepID: "done", ResourceVersion: "sv-1", Nonce: "n1", MessageID: "m1",
	}))
	return prompts.StepButtonID("s1", prompts.StepKey("review", "done"), "n1")
}

func TestStaleStepButtonsApproveNothing(t *testing.T) {
	f := newFixture(t)
	awaitingStep(f) // the session awaits approval of Review → Done
	current := reviewButton(t, f)
	stale := map[string]string{
		"another transition": prompts.StepButtonID("s1", prompts.StepKey("review", "deploy"), "n1"),
		"an earlier prompt":  prompts.StepButtonID("s1", prompts.StepKey("review", "done"), "n0"),
	}
	for name, button := range stale {
		got := f.run(f.button(opUser, button))
		assert.Contains(t, got, "out of date", name)
	}
	assert.Empty(t, f.host.Receipts)
	assert.Empty(t, f.host.Approvals)
	assert.Contains(t, f.syncs, "t1", "the task is re-synced so the stale prompt is cleared")

	awaitingStepAt(f, "sv-2") // a later visit to Review, before the prompt was refreshed
	assert.Contains(t, f.run(f.button(opUser, current)), "out of date")
	assert.Empty(t, f.host.Approvals)

	awaitingStep(f)
	f.run(f.button(opUser, current))
	assert.Len(t, f.host.Approvals, 1)
}

func TestLinkRelaysTheCodeAndNeverEchoesIt(t *testing.T) {
	f := newFixture(t)
	got := f.run(f.command(guestUser, CmdLink, opt(OptCode, " ABCD-EFGH-JKMN ")))
	require.Len(t, f.host.Links, 1)
	assert.Equal(t, pluginsdk.ExternalIdentityLinkRequest{RequestID: f.host.Links[0].RequestID, Code: "ABCD-EFGH-JKMN", Provider: "discord", Subject: guestUser, DisplayName: guestUser}, f.host.Links[0])
	assert.Contains(t, got, "Linked your Discord account to **Alice**")
	assert.NotContains(t, got, "ABCD")

	f.host.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandDenied, Reason: "missing_capability_approval"}, pluginsdk.ExternalIdentityLink{}, nil
	}
	got = f.run(f.command(guestUser, CmdLink, opt(OptCode, "ABCD-EFGH-JKMN")))
	assert.Contains(t, got, "host.v2.write:human_responses")
	assert.Contains(t, got, "code stays valid")

	f.host.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandDenied, Reason: "link_code_invalid_or_used"}, pluginsdk.ExternalIdentityLink{}, nil
	}
	got = f.run(f.command(guestUser, CmdLink, opt(OptCode, "ABCD-EFGH-JKMN")))
	assert.True(t, strings.HasPrefix(got, "That link code is invalid, expired, or already used."))

	outside := f.command(guestUser, CmdLink, opt(OptCode, "X"))
	outside.ChannelID = "general"
	f.host.LinkFn = nil
	assert.Contains(t, f.run(outside), "Linked", "/link works in any channel of a bound guild")
}

func TestAutocompleteListsPermissionsAndSteps(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	awaitingStep(f)
	i := f.interaction(opUser, discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{
		Name: CmdApprove, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: OptRequest, Type: discordgo.ApplicationCommandOptionString, Value: "", Focused: true}},
	})
	f.r.Handle(context.Background(), i)
	resp := f.api.Interactions[i.ID][0].(*discordgo.InteractionResponse)
	assert.Equal(t, discordgo.InteractionApplicationCommandAutocompleteResult, resp.Type)
	require.Len(t, resp.Data.Choices, 2)
	assert.Equal(t, "Permission: Run tests?", resp.Data.Choices[0].Name)
	assert.Equal(t, "perm:i1", resp.Data.Choices[0].Value)
	assert.Equal(t, "Step: Review → Done", resp.Data.Choices[1].Name)
	assert.Equal(t, "step:s1:"+prompts.StepKey("review", "done"), resp.Data.Choices[1].Value)
}
