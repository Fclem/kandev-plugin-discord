package kandev

import (
	"context"
	"errors"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Fake stands in for Kandev in every package's tests, so its Host
// semantics (filters, NotFound errors, default APPLIED outcomes, scripted
// hooks) are part of its contract.

func TestFakeTaskAndMessageReads(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	f.AddTask(pluginsdk.Task{ID: "b", WorkspaceID: "ws"})
	f.AddTask(pluginsdk.Task{ID: "a", WorkspaceID: "ws"})
	f.AddTask(pluginsdk.Task{ID: "e", WorkspaceID: "ws", IsEphemeral: true})
	f.AddTask(pluginsdk.Task{ID: "o", WorkspaceID: "other"})

	got, err := f.ListTasks(ctx, "ws", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, taskIDs(got), "sorted, ephemeral excluded")
	got, _ = f.ListTasks(ctx, "ws", true)
	assert.Equal(t, []string{"a", "b", "e"}, taskIDs(got))

	task, err := f.GetTask(ctx, "a")
	require.NoError(t, err)
	task.Title = "mutated"
	again, _ := f.GetTask(ctx, "a")
	assert.Empty(t, again.Title, "callers get copies")
	_, err = f.GetTask(ctx, "missing")
	assert.True(t, IsNotFound(err))

	f.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "a", CreatedAt: "2026-01-01T00:00:00Z"})
	f.AddMessage(pluginsdk.Message{ID: "m2", TaskID: "a", CreatedAt: "2026-01-02T00:00:00Z"})
	all, _ := f.ListMessages(ctx, "a", "")
	assert.Len(t, all, 2)
	since, _ := f.ListMessages(ctx, "a", "2026-01-02T00:00:00Z")
	require.Len(t, since, 1, "since is inclusive")
	assert.Equal(t, "m2", since[0].ID)

	f.Steps["wf"] = []pluginsdk.WorkflowStep{{ID: "s"}}
	steps, _ := f.ListWorkflowSteps(ctx, "wf")
	assert.Len(t, steps, 1)
	assert.Equal(t, []string{"ListTasks", "ListTasks", "GetTask", "GetTask", "GetTask", "ListMessages", "ListMessages", "ListWorkflowSteps"}, f.Calls)
}

func TestFakeSessionsAndSendHooks(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	boom := errors.New("boom")
	f.Sessions["a"] = []pluginsdk.Session{{ID: "s1"}}
	s, err := f.ListSessions(ctx, "a")
	require.NoError(t, err)
	assert.Len(t, s, 1)
	f.SessionsErr = boom
	_, err = f.ListSessions(ctx, "a")
	assert.ErrorIs(t, err, boom)

	d, err := f.SendMessage(ctx, "a", "hi")
	require.NoError(t, err)
	assert.Equal(t, "sent", d.Status)
	f.SendFn = func(string, string) (*pluginsdk.MessageDispatch, error) { return nil, boom }
	_, err = f.SendMessage(ctx, "a", "lost")
	assert.ErrorIs(t, err, boom)
	f.SendFn = func(string, string) (*pluginsdk.MessageDispatch, error) {
		return &pluginsdk.MessageDispatch{Status: "queued"}, nil
	}
	d, _ = f.SendMessage(ctx, "a", "later")
	assert.Equal(t, "queued", d.Status)
	assert.Equal(t, []SentMessage{{"a", "hi"}, {"a", "later"}}, f.Sent, "failed sends are not recorded")
}

func TestFakeInteractions(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	f.Interactions["i2"] = &pluginsdk.Interaction{ID: "i2", TaskID: "a", Status: pluginsdk.InteractionStatusPending}
	f.Interactions["i1"] = &pluginsdk.Interaction{ID: "i1", TaskID: "a", Status: pluginsdk.InteractionStatusPending}
	f.Interactions["done"] = &pluginsdk.Interaction{ID: "done", TaskID: "a", Status: pluginsdk.InteractionStatusApproved}

	pending, err := f.ListPendingInteractions(ctx, "a")
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, "i1", pending[0].ID)

	got, err := f.GetInteraction(ctx, "i1")
	require.NoError(t, err)
	assert.Equal(t, "a", got.TaskID)
	_, err = f.GetInteraction(ctx, "missing")
	assert.True(t, IsNotFound(err))

	obs, err := f.ExactInteraction(ctx, "ws", "i1")
	require.NoError(t, err)
	assert.Equal(t, "rev-i1", obs.ResourceVersion, "default revision")
	f.Revisions["i1"] = "rev-7"
	obs, _ = f.ExactInteraction(ctx, "ws", "i1")
	assert.Equal(t, "rev-7", obs.ResourceVersion)
	_, err = f.ExactInteraction(ctx, "ws", "missing")
	assert.True(t, IsNotFound(err))

	res, inter, err := f.RespondPermission(ctx, pluginsdk.ExactPermissionResponse{InteractionID: "i1"})
	require.NoError(t, err)
	assert.Equal(t, pluginsdk.CommandApplied, res.Status)
	assert.Equal(t, pluginsdk.InteractionStatusApproved, inter.Status)
	pending, _ = f.ListPendingInteractions(ctx, "a")
	assert.Len(t, pending, 1, "the answered interaction is no longer pending")
	res, _, _ = f.RespondPermission(ctx, pluginsdk.ExactPermissionResponse{InteractionID: "missing"})
	assert.Equal(t, pluginsdk.CommandNotFound, res.Status)

	boom := errors.New("boom")
	f.PendingErr = boom
	_, err = f.ListPendingInteractions(ctx, "a")
	assert.ErrorIs(t, err, boom)
	f.ExactErr = boom
	_, err = f.ExactInteraction(ctx, "ws", "i1")
	assert.ErrorIs(t, err, boom)
	_, err = f.ExactSessions(ctx, "ws", "a")
	assert.ErrorIs(t, err, boom)
	f.ExactErr = nil
	f.Exact["a"] = []pluginsdk.ExactSessionObservation{{ResourceVersion: "v"}}
	exact, _ := f.ExactSessions(ctx, "ws", "a")
	assert.Len(t, exact, 1)
}

func TestFakeCapability(t *testing.T) {
	f := NewFake()
	c, err := f.Capability(context.Background(), "ws")
	require.NoError(t, err)
	assert.Equal(t, uint64(7), c.ApprovalRevision)
	c.ApprovalRevision = 99
	again, _ := f.Capability(context.Background(), "ws")
	assert.Equal(t, uint64(7), again.ApprovalRevision, "callers get copies")
	f.CapabilityErr = errors.New("denied")
	_, err = f.Capability(context.Background(), "ws")
	assert.Error(t, err)
}

func TestFakeCommandsDefaultToAppliedAndRecord(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	cmd := pluginsdk.ExactTaskExecutionCommand{TaskID: "a", SessionID: "s"}

	res, ok, err := f.StopTaskRun(ctx, cmd)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, pluginsdk.CommandApplied, res.Status)
	_, ok, _ = f.CancelTurn(ctx, cmd)
	assert.True(t, ok)
	step, _ := f.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{SessionID: "s"})
	assert.Equal(t, "s", step.SessionID)
	res, _, _ = f.ApproveStep(ctx, pluginsdk.ExactStepApprovalCommand{SessionID: "s"})
	assert.Equal(t, pluginsdk.CommandApplied, res.Status)
	_, link, _ := f.LinkExternalIdentity(ctx, pluginsdk.ExternalIdentityLinkRequest{Provider: "discord", Subject: "42"})
	assert.Equal(t, "42", link.Subject)
	assert.NotEmpty(t, link.UserID)
	res, _, _ = f.RequestHumanResponseReceipt(ctx, pluginsdk.HumanResponseReceiptRequest{TargetID: "i"})
	assert.Equal(t, pluginsdk.CommandApplied, res.Status)

	assert.Len(t, f.Stops, 1)
	assert.Len(t, f.Cancels, 1)
	assert.Len(t, f.Approvals, 1)
	assert.Len(t, f.Links, 1)
	assert.Len(t, f.Receipts, 1)
}

func TestFakeCommandHooksScriptOutcomes(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	rejected := &pluginsdk.CommandResult{Status: pluginsdk.CommandDenied}
	f.StopFn = func(pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
		return rejected, false, nil
	}
	f.CancelFn = func(pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
		return rejected, false, nil
	}
	f.StepFn = func(pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
		return pluginsdk.StepApproval{Awaiting: true}, nil
	}
	f.ApproveFn = func(pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error) {
		return rejected, pluginsdk.StepApprovalResult{}, nil
	}
	f.RespondFn = func(pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error) {
		return rejected, nil, nil
	}
	f.LinkFn = func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
		return rejected, pluginsdk.ExternalIdentityLink{}, nil
	}
	f.ReceiptFn = func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
		return rejected, pluginsdk.HumanResponseReceipt{}, nil
	}

	cmd := pluginsdk.ExactTaskExecutionCommand{}
	r1, _, _ := f.StopTaskRun(ctx, cmd)
	r2, _, _ := f.CancelTurn(ctx, cmd)
	step, _ := f.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{})
	r3, _, _ := f.ApproveStep(ctx, pluginsdk.ExactStepApprovalCommand{})
	r4, _, _ := f.RespondPermission(ctx, pluginsdk.ExactPermissionResponse{})
	r5, _, _ := f.LinkExternalIdentity(ctx, pluginsdk.ExternalIdentityLinkRequest{})
	r6, _, _ := f.RequestHumanResponseReceipt(ctx, pluginsdk.HumanResponseReceiptRequest{})
	assert.True(t, step.Awaiting)
	for _, r := range []*pluginsdk.CommandResult{r1, r2, r3, r4, r5, r6} {
		assert.Equal(t, pluginsdk.CommandDenied, r.Status)
	}
	assert.Len(t, f.Responses, 1, "scripted calls are still recorded")
}

func TestNewRequestIDIsUnique(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	assert.Len(t, a, 36)
	assert.NotEqual(t, a, b)
}
