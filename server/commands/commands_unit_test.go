package commands

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/prompts"
	"kandev-plugin-discord/server/store"
)

func receiptOutcome(st pluginsdk.CommandStatus, reason string) func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
	return func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return &pluginsdk.CommandResult{Status: st, Reason: reason}, pluginsdk.HumanResponseReceipt{}, nil
	}
}

func TestRefusalReplies(t *testing.T) {
	cases := []struct {
		status  pluginsdk.CommandStatus
		reason  string
		want    string
		resolve bool
	}{
		{pluginsdk.CommandDenied, "external_identity_not_linked", "Your Discord account is not linked to a Kandev user, so nothing changed. Answer in Kandev: https://kandev.example.com/t/t1\n" + msgLinkHowTo, false},
		{pluginsdk.CommandDenied, "linked_user_not_authorized", "Your linked Kandev account cannot control sessions in this workspace, so nothing changed.", false},
		{pluginsdk.CommandDenied, "stale_capability_revision", "Kandev's access settings just changed; try again.", false},
		{pluginsdk.CommandDenied, "capability_revoked", msgApprovalGrant, false},
		{pluginsdk.CommandDenied, "policy", "Kandev refused the response (policy); nothing changed.", false},
		{pluginsdk.CommandConflict, "step_approval_changed", "The task moved before the approval landed; check `/status`.", false},
		{pluginsdk.CommandConflict, "interaction_revision_changed", "The request changed; try again.", false},
		{pluginsdk.CommandNotFound, "", "That request was already resolved.", true},
		{pluginsdk.CommandUnsupported, "", "This is an Office approval stage; approve it in Kandev: https://kandev.example.com/t/t1", false},
		{pluginsdk.CommandRateLimited, "", "Kandev could not do that right now (rate_limited); try again.", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status)+"/"+tc.reason, func(t *testing.T) {
			f := newFixture(t)
			promptID := f.pendingPermission(t, "i1")
			f.host.ReceiptFn = receiptOutcome(tc.status, tc.reason)
			assert.Equal(t, tc.want, f.run(f.button(opUser, prompts.PermissionButtonID("i1", 0))))
			assert.Empty(t, f.host.Responses, "no response without a receipt")
			resolved := strings.Contains(f.api.Message(channelID, promptID).Content, "Resolved elsewhere")
			assert.Equal(t, tc.resolve, resolved)
		})
	}
}

func TestRefusalOnHostErrors(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	button := prompts.PermissionButtonID("i1", 0)

	f.host.ReceiptFn = func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return nil, pluginsdk.HumanResponseReceipt{}, status.Error(codes.Unimplemented, "old host")
	}
	assert.Equal(t, "This Kandev version cannot take approvals from Discord; answer in Kandev: https://kandev.example.com/t/t1", f.run(f.button(opUser, button)))

	f.host.ReceiptFn = func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return nil, pluginsdk.HumanResponseReceipt{}, status.Error(codes.PermissionDenied, "grant")
	}
	assert.Equal(t, msgApprovalGrant, f.run(f.button(opUser, button)))

	f.host.ReceiptFn = func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return nil, pluginsdk.HumanResponseReceipt{}, nil
	}
	assert.Equal(t, "Kandev returned no result; try again.", f.run(f.button(opUser, button)))
}

func TestResponseRefusalAfterReceipt(t *testing.T) {
	f := newFixture(t)
	promptID := f.pendingPermission(t, "i1")
	f.host.RespondFn = func(pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandNotFound}, &pluginsdk.Interaction{Status: pluginsdk.InteractionStatusRejected,
			ResolvedBy: &pluginsdk.InteractionActor{DisplayName: "Bob"}}, nil
	}
	assert.Equal(t, "That request was already resolved.", f.run(f.button(opUser, prompts.PermissionButtonID("i1", 0))))
	assert.Contains(t, f.api.Message(channelID, promptID).Content, "✅ Denied by Bob.")
}

func TestHostErrorText(t *testing.T) {
	assert.Equal(t, "grant", hostErrorText(status.Error(codes.PermissionDenied, ""), "grant"))
	assert.Equal(t, msgUnsupported, hostErrorText(status.Error(codes.Unimplemented, ""), "grant"))
	assert.Equal(t, "Kandev no longer has that task or request.", hostErrorText(status.Error(codes.NotFound, ""), "grant"))
	assert.Equal(t, "Kandev is unavailable right now; try again.", hostErrorText(status.Error(codes.Unavailable, ""), "grant"))
}

func TestPermissionLookupOutcomes(t *testing.T) {
	f := newFixture(t)
	promptID := f.pendingPermission(t, "i1")
	button := prompts.PermissionButtonID("i1", 0)

	f.host.Interactions["i1"].TaskID = "t2"
	assert.Equal(t, "That request belongs to another task.", f.run(f.button(opUser, button)))
	f.host.Interactions["i1"].TaskID = "t1"

	assert.Equal(t, "This request has no matching option here; answer it in Kandev: https://kandev.example.com/t/t1",
		f.run(f.button(opUser, prompts.PermissionButtonID("i1", 9))))

	f.host.ExactErr = status.Error(codes.Unavailable, "down")
	assert.Equal(t, "Kandev is unavailable right now; try again.", f.run(f.button(opUser, button)))
	f.host.ExactErr = nil

	delete(f.host.Interactions, "i1")
	assert.Equal(t, "That request no longer exists in Kandev.", f.run(f.button(opUser, button)))
	assert.Contains(t, f.api.Message(channelID, promptID).Content, "⏹️ No longer pending.")
	stored, err := f.st.Prompts(context.Background(), "t1")
	require.NoError(t, err)
	assert.Empty(t, stored.Items)
	assert.Empty(t, f.host.Receipts)
}

func TestDenyWithoutARejectOption(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	f.host.Interactions["i1"].Options = []pluginsdk.InteractionOption{{OptionID: "a", Label: "Allow", Kind: "allow_always"}}
	assert.Contains(t, f.run(f.command(opUser, CmdDeny)), "no matching option")

	f.host.Interactions["i1"].Options = []pluginsdk.InteractionOption{{OptionID: "x", Kind: "custom"}, {OptionID: "r", Label: "Reject forever", Kind: "reject_always"}}
	f.run(f.command(opUser, CmdDeny))
	require.Len(t, f.host.Responses, 1)
	assert.Equal(t, "r", f.host.Responses[0].OptionID, "any reject_* option when no reject_once")
}

func TestCapabilityRefusals(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	f.host.CapabilityErr = status.Error(codes.Unavailable, "down")
	assert.Equal(t, "Kandev is unavailable right now; try again.", f.run(f.button(opUser, prompts.PermissionButtonID("i1", 0))))

	awaitingStep(f)
	assert.Equal(t, "Kandev is unavailable right now; try again.", f.run(f.button(opUser, reviewButton(t, f))))
	f.host.CapabilityErr = nil
	f.host.Capabilities.ApprovalRevision = 0
	assert.Equal(t, msgApprovalGrant, f.run(f.button(opUser, reviewButton(t, f))))
	assert.Empty(t, f.host.Receipts)
}

func TestApproveRequestOption(t *testing.T) {
	f := newFixture(t)
	awaitingStep(f)
	assert.Equal(t, "Pick a request from the list.", f.run(f.command(opUser, CmdApprove, opt(OptRequest, "garbage"))))
	assert.Equal(t, "Pick a request from the list.", f.run(f.command(opUser, CmdDeny, opt(OptRequest, "step:s1:"+prompts.StepKey("review", "done")))),
		"/deny never applies to steps")
	assert.Empty(t, f.host.Approvals)

	f.run(f.command(opUser, CmdApprove, opt(OptRequest, "step:s1:"+prompts.StepKey("review", "done"))))
	require.Len(t, f.host.Approvals, 1)
	assert.Equal(t, "s1", f.host.Approvals[0].SessionID)

	got := f.run(f.command(opUser, CmdApprove, opt(OptRequest, "step:s1:"+prompts.StepKey("review", "deploy"))))
	assert.Contains(t, got, "out of date")
}

func TestApproveLookupFailures(t *testing.T) {
	f := newFixture(t)
	f.host.PendingErr = status.Error(codes.PermissionDenied, "no read grant")
	assert.Equal(t, msgApprovalGrant, f.run(f.command(opUser, CmdApprove)))
	f.host.PendingErr = nil

	f.host.SessionsErr = status.Error(codes.Unavailable, "down")
	assert.Equal(t, "Kandev is unavailable right now; try again.", f.run(f.command(opUser, CmdApprove)))
	f.host.SessionsErr = nil

	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s0", State: "RUNNING"}, {ID: "s1", State: "WAITING_FOR_INPUT"}}
	f.host.StepFn = func(pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{}, status.Error(codes.Unimplemented, "office")
	}
	assert.Contains(t, f.run(f.command(opUser, CmdApprove)), "Office approval stage")
	assert.Equal(t, "Only members with the operator role can do that.", f.run(f.command(guestUser, CmdApprove)))
}

func TestStepApprovalRefusals(t *testing.T) {
	f := newFixture(t)
	awaitingStep(f)
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, TaskID: "t2", Awaiting: true}, nil
	}
	assert.Equal(t, "That step belongs to another task.", f.run(f.command(opUser, CmdApprove)))

	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, Awaiting: false}, nil
	}
	f.syncs = nil
	assert.Equal(t, msgNothingAwaiting, f.run(f.button(opUser, reviewButton(t, f))))
	assert.Equal(t, []string{"t1"}, f.syncs, "a stale prompt is re-synced")

	awaitingStep(f)
	f.host.ReceiptFn = receiptOutcome(pluginsdk.CommandDenied, "external_identity_not_linked")
	assert.Contains(t, f.run(f.command(opUser, CmdApprove)), "not linked")
	assert.Empty(t, f.host.Approvals)
}

func TestStepPromptCheckFailureApprovesNothing(t *testing.T) {
	f := newFixture(t)
	awaitingStep(f)
	button := reviewButton(t, f)
	f.r.Store = store.New(&failingState{MemoryHost: store.NewMemoryHost(), failKey: "step_approval"})
	// The channel must still look open: copy it into the new store.
	require.NoError(t, f.r.Store.PutChannel(context.Background(), store.Channel{TaskID: "t1", WorkspaceID: wsID, GuildID: guildID, ChannelID: channelID, Status: store.StatusActive}, ""))
	assert.Equal(t, "Kandev could not check this prompt right now; try again.", f.run(f.button(opUser, button)))
	assert.Empty(t, f.host.Receipts)
}

func TestStepApprovalWithoutTargetName(t *testing.T) {
	f := newFixture(t)
	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s1", State: "IDLE"}}
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{SessionID: q.SessionID, Awaiting: true, FromStepID: "review", FromStepName: "Review"}, nil
	}
	f.host.ReceiptFn = func(r pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, pluginsdk.HumanResponseReceipt{ID: "r", Actor: pluginsdk.InteractionActor{DisplayName: "Ann"}}, nil
	}
	assert.Equal(t, "✅ Step **Review** approved, recorded in Kandev as Ann via Discord.", f.run(f.command(opUser, CmdApprove)))
}

func TestButtonGuards(t *testing.T) {
	f := newFixture(t)
	f.pendingPermission(t, "i1")
	assert.Equal(t, "Only members with the operator role can do that.", f.run(f.button(guestUser, prompts.PermissionButtonID("i1", 0))))
	assert.Equal(t, "Unknown button.", f.run(f.button(opUser, "kd:other:1")))

	elsewhere := f.button(opUser, prompts.PermissionButtonID("i1", 0))
	elsewhere.ChannelID = "999"
	assert.Equal(t, "This prompt is no longer attached to a task channel.", f.run(elsewhere))
	assert.Empty(t, f.host.Receipts)
}

func TestUnknownCommandAndClosedChannel(t *testing.T) {
	f := newFixture(t)
	assert.Equal(t, "Unknown command.", f.run(f.command(opUser, "dance")))

	require.NoError(t, f.st.PutChannel(context.Background(), store.Channel{TaskID: "t1", WorkspaceID: wsID, GuildID: guildID, ChannelID: channelID, Status: store.StatusArchived}, channelID))
	assert.Equal(t, "This channel is closed; act on the task in Kandev: https://kandev.example.com/t/t1", f.run(f.command(opUser, CmdStop)))
	assert.Contains(t, f.run(f.command(guestUser, CmdStatus)), "**KAN-1 · Fix login**", "/status works in archived channels")
}

func TestOpenCheckFailure(t *testing.T) {
	f := newFixture(t)
	failing := &failingState{MemoryHost: store.NewMemoryHost(), failKey: "channel"}
	f.r.Store = store.New(failing)
	require.NoError(t, failing.MemoryHost.SetState(context.Background(), "instance", "", "channel:"+channelID,
		map[string]any{"task_id": "t1", "workspace_id": wsID, "guild_id": guildID}))
	assert.Equal(t, "Kandev could not check this channel right now; try again.", f.run(f.command(opUser, CmdStop)))
}

func TestUnboundAutocompleteIsIgnored(t *testing.T) {
	f := newFixture(t)
	i := f.interaction(opUser, discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{Name: CmdApprove})
	i.GuildID = "999999999999999999"
	f.r.Handle(context.Background(), i)
	assert.Empty(t, f.api.Calls(), "autocomplete in an unbound guild gets no answer")
}

func TestFailedDeferStopsTheInteraction(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("InteractionRespond", &discordapi.Error{Status: 404, Code: discordapi.CodeUnknownInteraction})
	f.host.Exact["t1"] = []pluginsdk.ExactSessionObservation{running("s1")}
	f.run(f.command(opUser, CmdStop))
	assert.Empty(t, f.host.Cancels, "nothing is done for an interaction that expired")
	assert.Empty(t, f.api.CallsTo("InteractionEdit"))
}

func TestUnboundReplyFailureIsTolerated(t *testing.T) {
	f := newFixture(t)
	f.api.FailNext("InteractionRespond", &discordapi.Error{Status: 500})
	f.api.FailNext("InteractionEdit", &discordapi.Error{Status: 500})
	unbound := f.command(opUser, CmdStatus)
	unbound.GuildID = "999999999999999999"
	assert.Empty(t, f.run(unbound))
	assert.Empty(t, f.run(f.command(guestUser, CmdStatus)), "a failed edit leaves no reply")
}

func TestAutocompleteFiltersTruncatesAndCaps(t *testing.T) {
	f := newFixture(t)
	for i := range 30 {
		id := fmt.Sprintf("i%02d", i)
		f.host.Interactions[id] = &pluginsdk.Interaction{ID: id, Kind: pluginsdk.InteractionKindPermission, TaskID: "t1",
			Status: pluginsdk.InteractionStatusPending, Title: "Run " + strings.Repeat("x", 120)}
	}
	f.host.Interactions["q"] = &pluginsdk.Interaction{ID: "q", Kind: pluginsdk.InteractionKindClarification, TaskID: "t1", Status: pluginsdk.InteractionStatusPending, Title: "Run question"}
	awaitingStep(f)
	complete := func(cmd, typed string) []*discordgo.ApplicationCommandOptionChoice {
		i := f.interaction(opUser, discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{
			Name: cmd, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: OptRequest, Type: discordgo.ApplicationCommandOptionString, Value: typed, Focused: true}},
		})
		f.r.Handle(context.Background(), i)
		return f.api.Interactions[i.ID][0].(*discordgo.InteractionResponse).Data.Choices
	}
	all := complete(CmdApprove, "")
	assert.Len(t, all, 25, "Discord accepts at most 25 choices")
	for _, c := range all {
		assert.LessOrEqual(t, utf8.RuneCountInString(c.Name), 100)
		assert.NotContains(t, c.Name, "question", "clarifications cannot be answered from Discord")
	}
	steps := complete(CmdApprove, "STEP")
	require.Len(t, steps, 1, "typed text filters case-insensitively")
	assert.Equal(t, "Step: Review → Done", steps[0].Name)
	assert.Empty(t, complete(CmdDeny, "step"), "/deny lists no steps")

	f.host.SessionsErr = status.Error(codes.Unavailable, "down")
	assert.Empty(t, complete(CmdApprove, "step"))
}

func TestAutocompleteOutsideTaskChannels(t *testing.T) {
	f := newFixture(t)
	i := f.interaction(opUser, discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{Name: CmdApprove})
	i.ChannelID = "999"
	f.api.FailNext("InteractionRespond", &discordapi.Error{Status: 500})
	f.r.Handle(context.Background(), i)
	assert.Len(t, f.api.CallsTo("InteractionRespond"), 1, "an empty list is still answered")
	assert.Empty(t, f.host.Calls)
}

func TestAutocompleteSkipsStepsNotAwaiting(t *testing.T) {
	f := newFixture(t)
	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s0", State: "RUNNING"}, {ID: "s1", State: "IDLE"}, {ID: "s2", State: "IDLE"}}
	f.host.StepFn = func(q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		if q.SessionID == "s1" {
			return pluginsdk.StepApproval{}, status.Error(codes.NotFound, "gone")
		}
		return pluginsdk.StepApproval{Awaiting: true, FromStepID: "a", FromStepName: "Plan"}, nil
	}
	i := f.interaction(opUser, discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{Name: CmdApprove})
	f.r.Handle(context.Background(), i)
	choices := f.api.Interactions[i.ID][0].(*discordgo.InteractionResponse).Data.Choices
	require.Len(t, choices, 1)
	assert.Equal(t, "Step: Plan", choices[0].Name)
	assert.Equal(t, "step:s2:"+prompts.StepKey("a", ""), choices[0].Value)
}

func TestLinkOutcomes(t *testing.T) {
	f := newFixture(t)
	assert.Equal(t, "Provide the code from Kandev. "+msgLinkHowTo, f.run(f.command(guestUser, CmdLink, opt(OptCode, "  "))))
	assert.Empty(t, f.host.Links)

	f.host.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return nil, pluginsdk.ExternalIdentityLink{}, status.Error(codes.PermissionDenied, "grant")
	}
	assert.Equal(t, msgApprovalGrant, f.run(f.command(guestUser, CmdLink, opt(OptCode, "C"))))

	f.host.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return nil, pluginsdk.ExternalIdentityLink{}, nil
	}
	assert.Equal(t, "Kandev returned no result; try again.", f.run(f.command(guestUser, CmdLink, opt(OptCode, "C"))))

	f.host.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandUnavailable}, pluginsdk.ExternalIdentityLink{}, nil
	}
	assert.Equal(t, "Kandev could not link the account right now; try again.", f.run(f.command(guestUser, CmdLink, opt(OptCode, "C"))))

	f.host.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandAlreadyApplied}, pluginsdk.ExternalIdentityLink{}, nil
	}
	assert.Contains(t, f.run(f.command(guestUser, CmdLink, opt(OptCode, "C"))), "to **your Kandev user**")
}

func TestStatusEdgeCases(t *testing.T) {
	f := newFixture(t)
	f.r.Config.KandevBaseURL = ""
	f.r.StepName = func(context.Context, *pluginsdk.Task) string { return "" }
	archivedAt := "2026-11-25T00:00:00Z"
	f.host.Tasks["t1"].ArchivedAt = &archivedAt
	f.host.Tasks["t1"].Identifier = ""
	got := f.run(f.command(guestUser, CmdStatus))
	assert.Equal(t, "**Fix login**\nState: in progress (archived)\nSessions: none yet\nQueued messages: 0 · Pending requests: 0", got)

	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s1", State: "IDLE", AgentProfileName: "Default"}, {ID: "s2", State: "IDLE"}}
	got = f.run(f.command(guestUser, CmdStatus))
	assert.Contains(t, got, "• Default — idle")
	assert.Contains(t, got, "• agent — idle")
	assert.Contains(t, got, "Queued messages: 4")

	f.host.SessionsErr = status.Error(codes.Unavailable, "down")
	f.host.PendingErr = status.Error(codes.Unavailable, "down")
	got = f.run(f.command(guestUser, CmdStatus))
	assert.Contains(t, got, "Sessions: unavailable")
	assert.Contains(t, got, "Pending requests: unavailable")
}

func TestStatusTaskReadFailures(t *testing.T) {
	f := newFixture(t)
	delete(f.host.Tasks, "t1")
	assert.Equal(t, "This task no longer exists in Kandev.", f.run(f.command(guestUser, CmdStatus)))

	f.r.Host = &unavailableTask{f.host}
	assert.Equal(t, "Kandev could not read the task right now; try again.", f.run(f.command(guestUser, CmdStatus)))
}

func TestStopRefusalsAndOutcomes(t *testing.T) {
	f := newFixture(t)
	noExec := running("s0")
	noExec.ExecutionID = ""
	profile := running("s1")
	profile.Session.AgentDisplayName, profile.Session.AgentProfileName = "", "Default"
	anon := running("s2")
	anon.Session.AgentDisplayName = ""
	f.host.Exact["t1"] = []pluginsdk.ExactSessionObservation{noExec, profile, anon}

	f.host.StopFn = func(cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
		if cmd.SessionID == "s1" {
			return &pluginsdk.CommandResult{Status: pluginsdk.CommandAlreadyApplied}, true, nil
		}
		return nil, false, nil
	}
	got := f.run(f.command(opUser, CmdStop, opt(OptScope, ScopeTask)))
	assert.Equal(t, "⏹️ Stopped the run of Default.\nKandev returned no result; check the task in Kandev.", got)
	assert.Len(t, f.host.Stops, 2, "sessions without an execution are skipped")
	assert.Equal(t, "discord:stop:task:", f.host.Stops[0].IdempotencyKey[:18])

	f.host.CancelFn = func(cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
		if cmd.SessionID == "s1" {
			return nil, false, status.Error(codes.Unavailable, "down")
		}
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandUncertain}, false, nil
	}
	got = f.run(f.command(opUser, CmdStop))
	assert.Equal(t, "Kandev is unavailable right now; try again.\nKandev could not do that right now (uncertain); try again.", got)

	f.host.CancelFn = nil
	f.host.Exact["t1"] = []pluginsdk.ExactSessionObservation{anon}
	assert.Equal(t, "⏹️ Cancelled the current turn of the agent; the session stays open.", f.run(f.command(opUser, CmdStop)))

	f.host.Capabilities.ApprovalRevision = 0
	assert.Equal(t, msgExecutionGrant, f.run(f.command(opUser, CmdStop)))
	f.host.CapabilityErr = status.Error(codes.PermissionDenied, "x")
	assert.Equal(t, msgExecutionGrant, f.run(f.command(opUser, CmdStop)))
}

func TestClipPrefersLineBreaks(t *testing.T) {
	text := strings.Repeat("a", 30) + "\n" + strings.Repeat("b", 30)
	got := clip(text, 50)
	assert.Equal(t, strings.Repeat("a", 30)+"\n… (more in Kandev)", got)
	assert.LessOrEqual(t, utf8.RuneCountInString(got), 50)

	noBreak := clip(strings.Repeat("c", 100), 50)
	assert.Equal(t, 50, utf8.RuneCountInString(noBreak))
	assert.Equal(t, "short", clip("short", 50))
}

var commandName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func TestDefinitionsAreValidForDiscord(t *testing.T) {
	defs := Definitions()
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
		assert.Regexp(t, commandName, d.Name)
		assert.NotEmpty(t, d.Description)
		assert.LessOrEqual(t, utf8.RuneCountInString(d.Description), 100, d.Name)
		for _, o := range d.Options {
			assert.Regexp(t, commandName, o.Name)
			assert.LessOrEqual(t, utf8.RuneCountInString(o.Description), 100, d.Name+"/"+o.Name)
			assert.False(t, o.Autocomplete && len(o.Choices) > 0, "Discord rejects autocomplete with choices")
		}
	}
	assert.ElementsMatch(t, []string{CmdLink, CmdStatus, CmdStop, CmdApprove, CmdDeny}, names)

	h := DefinitionsHash("123")
	assert.Len(t, h, 64)
	assert.Equal(t, h, DefinitionsHash("123"))
	assert.NotEqual(t, h, DefinitionsHash("456"), "another application registers its own commands")
}

// failingState fails reads of one key, like Host state for one record.
type failingState struct {
	*store.MemoryHost
	failKey string
}

func (f *failingState) GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	if key == f.failKey {
		return nil, false, status.Error(codes.Unavailable, "state down")
	}
	return f.MemoryHost.GetState(ctx, scope, scopeID, key)
}

// unavailableTask fails GetTask with a transient error.
type unavailableTask struct{ *kandev.Fake }

func (unavailableTask) GetTask(context.Context, string) (*pluginsdk.Task, error) {
	return nil, status.Error(codes.Unavailable, "down")
}
