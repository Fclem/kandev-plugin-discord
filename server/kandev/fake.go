package kandev

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewRequestID returns a fresh Host request ID.
func NewRequestID() string { return uuid.NewString() }

func newRequestID() string { return NewRequestID() }

// Fake is an in-memory Host for tests. Unset optional results default to
// APPLIED; set the *Fn hooks to script outcomes.
type Fake struct {
	mu sync.Mutex

	Tasks        map[string]*pluginsdk.Task
	Sessions     map[string][]pluginsdk.Session // task → sessions
	Messages     map[string][]pluginsdk.Message // task → messages, oldest first
	Interactions map[string]*pluginsdk.Interaction
	Steps        map[string][]pluginsdk.WorkflowStep // workflow → steps
	Exact        map[string][]pluginsdk.ExactSessionObservation
	Revisions    map[string]string // interaction → resource version

	Capabilities *pluginsdk.CapabilityContext

	SendFn        func(taskID, text string) (*pluginsdk.MessageDispatch, error)
	StopFn        func(pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error)
	CancelFn      func(pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error)
	StepFn        func(pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error)
	ApproveFn     func(pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error)
	RespondFn     func(pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error)
	LinkFn        func(pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error)
	ReceiptFn     func(pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error)
	SessionsErr   error
	PendingErr    error
	ExactErr      error
	CapabilityErr error

	Sent      []SentMessage
	Calls     []string
	Stops     []pluginsdk.ExactTaskExecutionCommand
	Cancels   []pluginsdk.ExactTaskExecutionCommand
	Approvals []pluginsdk.ExactStepApprovalCommand
	Responses []pluginsdk.ExactPermissionResponse
	Links     []pluginsdk.ExternalIdentityLinkRequest
	Receipts  []pluginsdk.HumanResponseReceiptRequest
}

// SentMessage is one recorded Send.
type SentMessage struct{ TaskID, Text string }

var _ Host = (*Fake)(nil)

// NewFake returns an empty Fake with a capability context.
func NewFake() *Fake {
	return &Fake{
		Tasks:        map[string]*pluginsdk.Task{},
		Sessions:     map[string][]pluginsdk.Session{},
		Messages:     map[string][]pluginsdk.Message{},
		Interactions: map[string]*pluginsdk.Interaction{},
		Steps:        map[string][]pluginsdk.WorkflowStep{},
		Exact:        map[string][]pluginsdk.ExactSessionObservation{},
		Revisions:    map[string]string{},
		Capabilities: &pluginsdk.CapabilityContext{ApprovalRevision: 7, ManifestDigest: "digest"},
	}
}

func applied() *pluginsdk.CommandResult {
	return &pluginsdk.CommandResult{Status: pluginsdk.CommandApplied}
}

func (f *Fake) call(name string) {
	f.Calls = append(f.Calls, name)
}

// AddTask stores a task.
func (f *Fake) AddTask(t pluginsdk.Task) *pluginsdk.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := t
	f.Tasks[t.ID] = &c
	return &c
}

// AddMessage appends a message to its task.
func (f *Fake) AddMessage(m pluginsdk.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Messages[m.TaskID] = append(f.Messages[m.TaskID], m)
}

func (f *Fake) GetTask(_ context.Context, taskID string) (*pluginsdk.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("GetTask")
	t, ok := f.Tasks[taskID]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	c := *t
	return &c, nil
}

func (f *Fake) ListTasks(_ context.Context, workspaceID string, includeEphemeral bool) ([]pluginsdk.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ListTasks")
	var out []pluginsdk.Task
	for _, t := range f.Tasks {
		if t.WorkspaceID == workspaceID && (includeEphemeral || !t.IsEphemeral) {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *Fake) ListSessions(_ context.Context, taskID string) ([]pluginsdk.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ListSessions")
	return slices.Clone(f.Sessions[taskID]), f.SessionsErr
}

func (f *Fake) ListMessages(_ context.Context, taskID, since string) ([]pluginsdk.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ListMessages")
	var out []pluginsdk.Message
	for _, m := range f.Messages[taskID] {
		if since == "" || m.CreatedAt >= since {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *Fake) SendMessage(_ context.Context, taskID, text string) (*pluginsdk.MessageDispatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("SendMessage")
	if f.SendFn != nil {
		d, err := f.SendFn(taskID, text)
		if err == nil {
			f.Sent = append(f.Sent, SentMessage{taskID, text})
		}
		return d, err
	}
	f.Sent = append(f.Sent, SentMessage{taskID, text})
	return &pluginsdk.MessageDispatch{Status: "sent"}, nil
}

func (f *Fake) ListPendingInteractions(_ context.Context, taskID string) ([]pluginsdk.Interaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ListPendingInteractions")
	if f.PendingErr != nil {
		return nil, f.PendingErr
	}
	var out []pluginsdk.Interaction
	for _, i := range f.Interactions {
		if i.TaskID == taskID && i.Status == pluginsdk.InteractionStatusPending {
			out = append(out, *i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (f *Fake) GetInteraction(_ context.Context, id string) (*pluginsdk.Interaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("GetInteraction")
	i, ok := f.Interactions[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "interaction not found")
	}
	c := *i
	return &c, nil
}

func (f *Fake) ListWorkflowSteps(_ context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ListWorkflowSteps")
	return slices.Clone(f.Steps[workflowID]), nil
}

func (f *Fake) Capability(context.Context, string) (*pluginsdk.CapabilityContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("Capability")
	if f.CapabilityErr != nil {
		return nil, f.CapabilityErr
	}
	c := *f.Capabilities
	return &c, nil
}

func (f *Fake) ExactSessions(_ context.Context, _, taskID string) ([]pluginsdk.ExactSessionObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ExactSessions")
	if f.ExactErr != nil {
		return nil, f.ExactErr
	}
	return slices.Clone(f.Exact[taskID]), nil
}

func (f *Fake) ExactInteraction(_ context.Context, _, interactionID string) (pluginsdk.ExactInteractionObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ExactInteraction")
	if f.ExactErr != nil {
		return pluginsdk.ExactInteractionObservation{}, f.ExactErr
	}
	i, ok := f.Interactions[interactionID]
	if !ok {
		return pluginsdk.ExactInteractionObservation{}, status.Error(codes.NotFound, "interaction not found")
	}
	rev := f.Revisions[interactionID]
	if rev == "" {
		rev = "rev-" + interactionID
	}
	return pluginsdk.ExactInteractionObservation{Interaction: *i, ResourceVersion: rev}, nil
}

func (f *Fake) StopTaskRun(_ context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stops = append(f.Stops, cmd)
	if f.StopFn != nil {
		return f.StopFn(cmd)
	}
	return applied(), true, nil
}

func (f *Fake) CancelTurn(_ context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Cancels = append(f.Cancels, cmd)
	if f.CancelFn != nil {
		return f.CancelFn(cmd)
	}
	return applied(), true, nil
}

func (f *Fake) GetStepApproval(_ context.Context, q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("GetStepApproval")
	if f.StepFn != nil {
		return f.StepFn(q)
	}
	return pluginsdk.StepApproval{SessionID: q.SessionID}, nil
}

func (f *Fake) ApproveStep(_ context.Context, cmd pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Approvals = append(f.Approvals, cmd)
	if f.ApproveFn != nil {
		return f.ApproveFn(cmd)
	}
	return applied(), pluginsdk.StepApprovalResult{}, nil
}

func (f *Fake) RespondPermission(_ context.Context, r pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Responses = append(f.Responses, r)
	if f.RespondFn != nil {
		return f.RespondFn(r)
	}
	i := f.Interactions[r.InteractionID]
	if i == nil {
		return &pluginsdk.CommandResult{Status: pluginsdk.CommandNotFound}, nil, nil
	}
	i.Status = pluginsdk.InteractionStatusApproved
	c := *i
	return applied(), &c, nil
}

func (f *Fake) LinkExternalIdentity(_ context.Context, r pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Links = append(f.Links, r)
	if f.LinkFn != nil {
		return f.LinkFn(r)
	}
	return applied(), pluginsdk.ExternalIdentityLink{Provider: r.Provider, Subject: r.Subject, UserID: "u1", UserDisplayName: "Alice"}, nil
}

func (f *Fake) RequestHumanResponseReceipt(_ context.Context, r pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Receipts = append(f.Receipts, r)
	if f.ReceiptFn != nil {
		return f.ReceiptFn(r)
	}
	return applied(), pluginsdk.HumanResponseReceipt{
		ID: "receipt-" + r.TargetID, Kind: r.Kind, TargetID: r.TargetID, ResourceVersion: r.ExpectedResourceVersion,
		Actor: pluginsdk.InteractionActor{UserID: "u1", DisplayName: "Alice", Via: "discord"},
	}, nil
}
