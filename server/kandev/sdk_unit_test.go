package kandev

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pages serves a cursor-paged listing: cursor → items and the next cursor
// (empty on the last page). It records the requested cursors.
type pages[T any] struct {
	mu      sync.Mutex
	byPage  map[string][]T
	next    map[string]string
	err     error
	cursors []string
	limits  []int32
}

func pagesOf[T any](chunks ...[]T) *pages[T] {
	p := &pages[T]{byPage: map[string][]T{}, next: map[string]string{}}
	cursor := ""
	for i, chunk := range chunks {
		p.byPage[cursor] = chunk
		if i < len(chunks)-1 {
			p.next[cursor] = fmt.Sprintf("c%d", i+1)
		}
		cursor = p.next[cursor]
	}
	return p
}

func (p *pages[T]) list(page pluginsdk.Page) ([]T, *pluginsdk.PageInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursors = append(p.cursors, page.Cursor)
	p.limits = append(p.limits, page.Limit)
	if p.err != nil {
		return nil, nil, p.err
	}
	next := p.next[page.Cursor]
	return p.byPage[page.Cursor], &pluginsdk.PageInfo{NextCursor: next, HasMore: next != ""}, nil
}

// sdkHost is a v1 pluginsdk.Host with the four data readers the facade
// uses. Any other Host call panics on the nil embedded interface.
type sdkHost struct {
	pluginsdk.Host
	tasks    *taskReader
	sessions *sessionReader
	messages *messageReader
	steps    *workflowReader
}

func (h *sdkHost) Tasks() pluginsdk.TaskReader         { return h.tasks }
func (h *sdkHost) Sessions() pluginsdk.SessionReader   { return h.sessions }
func (h *sdkHost) Messages() pluginsdk.MessageReader   { return h.messages }
func (h *sdkHost) Workflows() pluginsdk.WorkflowReader { return h.steps }

type taskReader struct {
	pluginsdk.TaskReader
	pages   *pages[pluginsdk.Task]
	filters []pluginsdk.TaskFilter
	byID    map[string]*pluginsdk.Task
}

func (r *taskReader) List(_ context.Context, f pluginsdk.TaskFilter, p pluginsdk.Page) ([]pluginsdk.Task, *pluginsdk.PageInfo, error) {
	r.filters = append(r.filters, f)
	return r.pages.list(p)
}

func (r *taskReader) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	if t, ok := r.byID[id]; ok {
		return t, nil
	}
	return nil, status.Error(codes.NotFound, "no task")
}

type sessionReader struct {
	pluginsdk.SessionReader
	pages   *pages[pluginsdk.Session]
	filters []pluginsdk.SessionFilter
}

func (r *sessionReader) List(_ context.Context, f pluginsdk.SessionFilter, p pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	r.filters = append(r.filters, f)
	return r.pages.list(p)
}

type messageReader struct {
	pluginsdk.MessageReader
	pages   *pages[pluginsdk.Message]
	filters []pluginsdk.MessageFilter
	sent    []string
}

func (r *messageReader) List(_ context.Context, f pluginsdk.MessageFilter, p pluginsdk.Page) ([]pluginsdk.Message, *pluginsdk.PageInfo, error) {
	r.filters = append(r.filters, f)
	return r.pages.list(p)
}

func (r *messageReader) Send(_ context.Context, taskID, sessionID, text string) (*pluginsdk.MessageDispatch, error) {
	r.sent = append(r.sent, taskID+"|"+sessionID+"|"+text)
	return &pluginsdk.MessageDispatch{Status: "queued"}, nil
}

type workflowReader struct {
	pluginsdk.WorkflowReader
	steps map[string][]pluginsdk.WorkflowStep
}

func (r *workflowReader) ListSteps(_ context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	return r.steps[workflowID], nil
}

func newSDKHost() *sdkHost {
	return &sdkHost{
		tasks:    &taskReader{pages: pagesOf[pluginsdk.Task](), byID: map[string]*pluginsdk.Task{}},
		sessions: &sessionReader{pages: pagesOf[pluginsdk.Session]()},
		messages: &messageReader{pages: pagesOf[pluginsdk.Message]()},
		steps:    &workflowReader{steps: map[string][]pluginsdk.WorkflowStep{}},
	}
}

// v2Host adds every optional extension the facade uses.
type v2Host struct {
	*sdkHost
	interactions *interactionAccessor
	queries      *exactQueries
	exec         *execCommands
	respond      *interactionCommands
	delegation   *delegation
	capability   *pluginsdk.CapabilityContext
}

func (h *v2Host) Interactions() pluginsdk.InteractionAccessor { return h.interactions }
func (h *v2Host) ExactQueries() pluginsdk.ExactQueryManager   { return h.queries }
func (h *v2Host) ExecutionCommands() pluginsdk.ExactExecutionCommandManager {
	return h.exec
}
func (h *v2Host) ExactInteractionCommands() pluginsdk.ExactInteractionCommandManager {
	return h.respond
}
func (h *v2Host) HumanResponseDelegation() pluginsdk.HumanResponseDelegationManager {
	return h.delegation
}
func (h *v2Host) GetCapabilityContext(_ context.Context, workspaceID string) (*pluginsdk.CapabilityContext, error) {
	c := *h.capability
	c.WorkspaceID = workspaceID
	return &c, nil
}
func (h *v2Host) UpdateTaskExact(context.Context, pluginsdk.ExactTaskUpdate) (*pluginsdk.CommandResult, *pluginsdk.Task, error) {
	panic("not used by the plugin")
}
func (h *v2Host) ManagedAgentConversations() pluginsdk.ManagedAgentConversationManager {
	panic("not used by the plugin")
}

type interactionAccessor struct {
	pluginsdk.InteractionAccessor
	pages   *pages[pluginsdk.Interaction]
	filters []pluginsdk.InteractionFilter
}

func (a *interactionAccessor) ListPending(_ context.Context, f pluginsdk.InteractionFilter, p pluginsdk.Page) ([]pluginsdk.Interaction, *pluginsdk.PageInfo, error) {
	a.filters = append(a.filters, f)
	return a.pages.list(p)
}

func (a *interactionAccessor) Get(_ context.Context, id string) (*pluginsdk.Interaction, error) {
	return &pluginsdk.Interaction{ID: id}, nil
}

type exactQueries struct {
	pluginsdk.ExactQueryManager
	sessionPages []pluginsdk.ExactSessionPage
	sessionQs    []pluginsdk.ExactSessionQuery
	interactionQ []pluginsdk.ExactInteractionGetQuery
	err          error
}

func (q *exactQueries) ListSessions(_ context.Context, query pluginsdk.ExactSessionQuery) (pluginsdk.ExactSessionPage, error) {
	q.sessionQs = append(q.sessionQs, query)
	if q.err != nil {
		return pluginsdk.ExactSessionPage{}, q.err
	}
	return q.sessionPages[len(q.sessionQs)-1], nil
}

func (q *exactQueries) GetInteraction(_ context.Context, query pluginsdk.ExactInteractionGetQuery) (pluginsdk.ExactInteractionObservation, pluginsdk.HostReadReceipt, error) {
	q.interactionQ = append(q.interactionQ, query)
	return pluginsdk.ExactInteractionObservation{Interaction: pluginsdk.Interaction{ID: query.InteractionID}, ResourceVersion: "rv-9"}, pluginsdk.HostReadReceipt{}, q.err
}

type execCommands struct {
	pluginsdk.ExactExecutionCommandManager
	calls []string
}

func (e *execCommands) StopTaskRun(_ context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
	e.calls = append(e.calls, "stop:"+cmd.TaskID)
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, true, nil
}

func (e *execCommands) CancelTurn(_ context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
	e.calls = append(e.calls, "cancel:"+cmd.SessionID)
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, false, nil
}

func (e *execCommands) GetStepApproval(_ context.Context, q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
	e.calls = append(e.calls, "step:"+q.SessionID)
	return pluginsdk.StepApproval{SessionID: q.SessionID, Awaiting: true}, nil
}

func (e *execCommands) ApproveStep(_ context.Context, cmd pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error) {
	e.calls = append(e.calls, "approve:"+cmd.HumanResponseReceiptID)
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, pluginsdk.StepApprovalResult{FromStepID: "a", ToStepID: "b"}, nil
}

type interactionCommands struct {
	pluginsdk.ExactInteractionCommandManager
	got []pluginsdk.ExactPermissionResponse
}

func (c *interactionCommands) RespondPermission(_ context.Context, r pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error) {
	c.got = append(c.got, r)
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, &pluginsdk.Interaction{ID: r.InteractionID}, nil
}

type delegation struct {
	links    []pluginsdk.ExternalIdentityLinkRequest
	receipts []pluginsdk.HumanResponseReceiptRequest
}

func (d *delegation) LinkExternalIdentity(_ context.Context, r pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
	d.links = append(d.links, r)
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, pluginsdk.ExternalIdentityLink{Subject: r.Subject, UserID: "u1"}, nil
}

func (d *delegation) RequestHumanResponseReceipt(_ context.Context, r pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
	d.receipts = append(d.receipts, r)
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}, pluginsdk.HumanResponseReceipt{ID: "rcpt-1", TargetID: r.TargetID}, nil
}

func newV2Host() *v2Host {
	return &v2Host{
		sdkHost:      newSDKHost(),
		interactions: &interactionAccessor{pages: pagesOf[pluginsdk.Interaction]()},
		queries:      &exactQueries{},
		exec:         &execCommands{},
		respond:      &interactionCommands{},
		delegation:   &delegation{},
		capability:   &pluginsdk.CapabilityContext{ApprovalRevision: 3},
	}
}

func TestSDKListTasksFollowsEveryPage(t *testing.T) {
	h := newSDKHost()
	h.tasks.pages = pagesOf([]pluginsdk.Task{{ID: "t1"}, {ID: "t2"}}, []pluginsdk.Task{{ID: "t3"}}, []pluginsdk.Task{{ID: "t4"}})
	got, err := SDK{H: h}.ListTasks(context.Background(), "ws-1", true)
	require.NoError(t, err)
	assert.Equal(t, []string{"t1", "t2", "t3", "t4"}, taskIDs(got))
	assert.Equal(t, []string{"", "c1", "c2"}, h.tasks.pages.cursors)
	assert.Equal(t, []int32{pageSize, pageSize, pageSize}, h.tasks.pages.limits)
	f := h.tasks.filters[0]
	assert.Equal(t, []string{"ws-1"}, f.WorkspaceIDs)
	assert.True(t, f.IncludeArchived, "archived tasks are listed so their channels can be archived")
	assert.True(t, f.IncludeEphemeral)

	h.tasks.filters = nil
	_, err = SDK{H: h}.ListTasks(context.Background(), "ws-1", false)
	require.NoError(t, err)
	assert.False(t, h.tasks.filters[0].IncludeEphemeral)
}

func TestSDKListingsStopOnMissingPageInfo(t *testing.T) {
	h := newSDKHost()
	h.tasks.pages = pagesOf([]pluginsdk.Task{{ID: "t1"}})
	// A HasMore page without a cursor would loop forever if followed.
	h.tasks.pages.next[""] = ""
	got, err := SDK{H: h}.ListTasks(context.Background(), "ws", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"t1"}, taskIDs(got))
	assert.Len(t, h.tasks.pages.cursors, 1)

	nilInfo := &nilInfoSessions{}
	h.sessions = &sessionReader{SessionReader: nilInfo}
	sessions, err := SDK{H: &nilInfoHost{sdkHost: h, r: nilInfo}}.ListSessions(context.Background(), "t1")
	require.NoError(t, err)
	assert.Len(t, sessions, 1)
	assert.Equal(t, 1, nilInfo.calls)
}

// nilInfoSessions answers one page with a nil PageInfo.
type nilInfoSessions struct {
	pluginsdk.SessionReader
	calls int
}

func (r *nilInfoSessions) List(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	r.calls++
	return []pluginsdk.Session{{ID: "s1"}}, nil, nil
}

type nilInfoHost struct {
	*sdkHost
	r *nilInfoSessions
}

func (h *nilInfoHost) Sessions() pluginsdk.SessionReader { return h.r }

func TestSDKListSessionsAndMessages(t *testing.T) {
	h := newSDKHost()
	h.sessions.pages = pagesOf([]pluginsdk.Session{{ID: "s1"}}, []pluginsdk.Session{{ID: "s2"}})
	h.messages.pages = pagesOf([]pluginsdk.Message{{ID: "m1"}}, []pluginsdk.Message{{ID: "m2"}, {ID: "m3"}})
	sdk := SDK{H: h}
	ctx := context.Background()

	sessions, err := sdk.ListSessions(ctx, "t1")
	require.NoError(t, err)
	assert.Len(t, sessions, 2)
	assert.Equal(t, []string{"t1"}, h.sessions.filters[0].TaskIDs)
	assert.Equal(t, []string{"", "c1"}, h.sessions.pages.cursors)

	msgs, err := sdk.ListMessages(ctx, "t1", "")
	require.NoError(t, err)
	assert.Len(t, msgs, 3)
	assert.Nil(t, h.messages.filters[0].Since, "empty since lists everything")
	assert.Equal(t, []string{"t1"}, h.messages.filters[0].TaskIDs)

	_, err = sdk.ListMessages(ctx, "t1", "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	since := h.messages.filters[len(h.messages.filters)-1].Since
	require.NotNil(t, since)
	assert.Equal(t, "2026-01-01T00:00:00Z", *since)

	d, err := sdk.SendMessage(ctx, "t1", "hello")
	require.NoError(t, err)
	assert.Equal(t, "queued", d.Status)
	assert.Equal(t, []string{"t1||hello"}, h.messages.sent, "sent to the task's primary session")
}

func TestSDKListingErrorsPropagate(t *testing.T) {
	boom := errors.New("host down")
	h := newSDKHost()
	h.tasks.pages.err = boom
	h.sessions.pages.err = boom
	h.messages.pages.err = boom
	sdk := SDK{H: h}
	ctx := context.Background()
	_, err := sdk.ListTasks(ctx, "ws", false)
	assert.ErrorIs(t, err, boom)
	_, err = sdk.ListSessions(ctx, "t")
	assert.ErrorIs(t, err, boom)
	_, err = sdk.ListMessages(ctx, "t", "")
	assert.ErrorIs(t, err, boom)

	v2 := newV2Host()
	v2.interactions.pages.err = boom
	_, err = SDK{H: v2}.ListPendingInteractions(ctx, "t")
	assert.ErrorIs(t, err, boom)
	v2.queries.err = boom
	_, err = SDK{H: v2}.ExactSessions(ctx, "ws", "t")
	assert.ErrorIs(t, err, boom)
	_, err = SDK{H: v2}.ExactInteraction(ctx, "ws", "i")
	assert.ErrorIs(t, err, boom)
}

func TestSDKGetTaskAndWorkflowSteps(t *testing.T) {
	h := newSDKHost()
	h.tasks.byID["t1"] = &pluginsdk.Task{ID: "t1", Title: "Fix"}
	h.steps.steps["wf"] = []pluginsdk.WorkflowStep{{ID: "s1"}, {ID: "s2"}}
	sdk := SDK{H: h}
	task, err := sdk.GetTask(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, "Fix", task.Title)
	_, err = sdk.GetTask(context.Background(), "missing")
	assert.True(t, IsNotFound(err))
	steps, err := sdk.ListWorkflowSteps(context.Background(), "wf")
	require.NoError(t, err)
	assert.Len(t, steps, 2)
}

func TestSDKExtensionsAreUnsupportedOnAV1Host(t *testing.T) {
	sdk := SDK{H: newSDKHost()}
	ctx := context.Background()
	cmd := pluginsdk.ExactTaskExecutionCommand{TaskID: "t"}
	errs := map[string]error{}
	_, errs["ListPendingInteractions"] = sdk.ListPendingInteractions(ctx, "t")
	_, errs["GetInteraction"] = sdk.GetInteraction(ctx, "i")
	_, errs["Capability"] = sdk.Capability(ctx, "ws")
	_, errs["ExactSessions"] = sdk.ExactSessions(ctx, "ws", "t")
	_, errs["ExactInteraction"] = sdk.ExactInteraction(ctx, "ws", "i")
	_, _, errs["StopTaskRun"] = sdk.StopTaskRun(ctx, cmd)
	_, _, errs["CancelTurn"] = sdk.CancelTurn(ctx, cmd)
	_, errs["GetStepApproval"] = sdk.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{})
	_, _, errs["ApproveStep"] = sdk.ApproveStep(ctx, pluginsdk.ExactStepApprovalCommand{})
	_, _, errs["RespondPermission"] = sdk.RespondPermission(ctx, pluginsdk.ExactPermissionResponse{})
	_, _, errs["LinkExternalIdentity"] = sdk.LinkExternalIdentity(ctx, pluginsdk.ExternalIdentityLinkRequest{})
	_, _, errs["RequestHumanResponseReceipt"] = sdk.RequestHumanResponseReceipt(ctx, pluginsdk.HumanResponseReceiptRequest{})
	for name, err := range errs {
		assert.ErrorIs(t, err, ErrUnsupported, name)
		assert.True(t, IsUnimplemented(err), name)
	}
}

func TestSDKNilExactQueriesAreUnsupported(t *testing.T) {
	h := newV2Host()
	h.queries = nil
	_, err := SDK{H: &nilQueriesHost{h}}.ExactSessions(context.Background(), "ws", "t")
	assert.ErrorIs(t, err, ErrUnsupported)
}

// nilQueriesHost advertises ExactQueries but returns a nil manager, like an
// SDK whose host lacks the service.
type nilQueriesHost struct{ *v2Host }

func (h *nilQueriesHost) ExactQueries() pluginsdk.ExactQueryManager { return nil }

func TestSDKPendingInteractionsPageAndGet(t *testing.T) {
	h := newV2Host()
	h.interactions.pages = pagesOf([]pluginsdk.Interaction{{ID: "i1"}}, []pluginsdk.Interaction{{ID: "i2"}})
	sdk := SDK{H: h}
	got, err := sdk.ListPendingInteractions(context.Background(), "t1")
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, []string{"t1"}, h.interactions.filters[0].TaskIDs)
	assert.Equal(t, []string{"", "c1"}, h.interactions.pages.cursors)

	one, err := sdk.GetInteraction(context.Background(), "i9")
	require.NoError(t, err)
	assert.Equal(t, "i9", one.ID)
}

func TestSDKExactSessionsCarrySnapshotAcrossPages(t *testing.T) {
	h := newV2Host()
	h.queries.sessionPages = []pluginsdk.ExactSessionPage{
		{Items: []pluginsdk.ExactSessionObservation{{ResourceVersion: "a"}}, PageInfo: pluginsdk.ExactReadPageInfo{HasMore: true, NextCursor: "n1", SnapshotVersion: "snap-1"}},
		{Items: []pluginsdk.ExactSessionObservation{{ResourceVersion: "b"}}, PageInfo: pluginsdk.ExactReadPageInfo{HasMore: true, NextCursor: ""}},
	}
	got, err := SDK{H: h}.ExactSessions(context.Background(), "ws-1", "t1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[1].ResourceVersion)

	qs := h.queries.sessionQs
	require.Len(t, qs, 2, "a HasMore page without cursor ends the listing")
	assert.Equal(t, "ws-1", qs[0].WorkspaceID)
	assert.Equal(t, []string{"t1"}, qs[0].Filter.TaskIDs)
	assert.Equal(t, pluginsdk.ExactReadPage{Limit: pageSize}, qs[0].Page)
	assert.Equal(t, pluginsdk.ExactReadPage{Limit: pageSize, Cursor: "n1", SnapshotVersion: "snap-1"}, qs[1].Page)
	assert.NotEmpty(t, qs[0].RequestID)
	assert.NotEqual(t, qs[0].RequestID, qs[1].RequestID, "every Host request has its own ID")
}

func TestSDKExactInteractionAndCapability(t *testing.T) {
	h := newV2Host()
	sdk := SDK{H: h}
	obs, err := sdk.ExactInteraction(context.Background(), "ws-1", "i1")
	require.NoError(t, err)
	assert.Equal(t, "rv-9", obs.ResourceVersion)
	q := h.queries.interactionQ[0]
	assert.Equal(t, "ws-1", q.WorkspaceID)
	assert.Equal(t, "i1", q.InteractionID)
	assert.NotEmpty(t, q.RequestID)

	cap, err := sdk.Capability(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(3), cap.ApprovalRevision)
	assert.Equal(t, "ws-1", cap.WorkspaceID)
}

func TestSDKCommandsReachTheExtensions(t *testing.T) {
	h := newV2Host()
	sdk := SDK{H: h}
	ctx := context.Background()

	_, stopped, err := sdk.StopTaskRun(ctx, pluginsdk.ExactTaskExecutionCommand{TaskID: "t1"})
	require.NoError(t, err)
	assert.True(t, stopped)
	_, cancelled, err := sdk.CancelTurn(ctx, pluginsdk.ExactTaskExecutionCommand{SessionID: "s1"})
	require.NoError(t, err)
	assert.False(t, cancelled)
	step, err := sdk.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{SessionID: "s1"})
	require.NoError(t, err)
	assert.True(t, step.Awaiting)
	_, res, err := sdk.ApproveStep(ctx, pluginsdk.ExactStepApprovalCommand{HumanResponseReceiptID: "r1"})
	require.NoError(t, err)
	assert.Equal(t, "b", res.ToStepID)
	assert.Equal(t, []string{"stop:t1", "cancel:s1", "step:s1", "approve:r1"}, h.exec.calls)

	_, inter, err := sdk.RespondPermission(ctx, pluginsdk.ExactPermissionResponse{InteractionID: "i1", OptionID: "allow"})
	require.NoError(t, err)
	assert.Equal(t, "i1", inter.ID)
	assert.Equal(t, "allow", h.respond.got[0].OptionID)

	_, link, err := sdk.LinkExternalIdentity(ctx, pluginsdk.ExternalIdentityLinkRequest{Provider: "discord", Subject: "42", Code: "c"})
	require.NoError(t, err)
	assert.Equal(t, "u1", link.UserID)
	assert.Equal(t, "c", h.delegation.links[0].Code)
	_, receipt, err := sdk.RequestHumanResponseReceipt(ctx, pluginsdk.HumanResponseReceiptRequest{TargetID: "i1", Kind: "permission"})
	require.NoError(t, err)
	assert.Equal(t, "rcpt-1", receipt.ID)
	assert.Equal(t, "permission", h.delegation.receipts[0].Kind)
}

func TestErrorClassifiers(t *testing.T) {
	assert.True(t, IsNotFound(status.Error(codes.NotFound, "x")))
	assert.False(t, IsNotFound(errors.New("x")))
	assert.True(t, IsPermissionDenied(status.Error(codes.PermissionDenied, "x")))
	assert.False(t, IsPermissionDenied(status.Error(codes.NotFound, "x")))
	assert.True(t, IsUnimplemented(status.Error(codes.Unimplemented, "x")))
	assert.True(t, IsUnimplemented(fmt.Errorf("wrapped: %w", ErrUnsupported)))
	assert.False(t, IsUnimplemented(nil))
}

func taskIDs(ts []pluginsdk.Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
