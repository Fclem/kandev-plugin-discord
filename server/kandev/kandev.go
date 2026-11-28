// Package kandev is the plugin's narrow facade over the Kandev Host: the v1
// data readers plus the exact v2 extensions merged in kdlbs/kandev#5438.
// Callers depend on Host so tests use Fake.
package kandev

import (
	"context"
	"errors"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrUnsupported means the running Kandev does not offer the extension.
var ErrUnsupported = errors.New("this Kandev version does not support the operation")

// Host is every Kandev call the plugin makes.
type Host interface {
	GetTask(ctx context.Context, taskID string) (*pluginsdk.Task, error)
	// ListTasks returns every task of a workspace, archived ones included.
	ListTasks(ctx context.Context, workspaceID string, includeEphemeral bool) ([]pluginsdk.Task, error)
	ListSessions(ctx context.Context, taskID string) ([]pluginsdk.Session, error)
	// ListMessages returns a task's messages created at or after since
	// (RFC3339, empty for all), oldest first.
	ListMessages(ctx context.Context, taskID, since string) ([]pluginsdk.Message, error)
	SendMessage(ctx context.Context, taskID, text string) (*pluginsdk.MessageDispatch, error)
	ListPendingInteractions(ctx context.Context, taskID string) ([]pluginsdk.Interaction, error)
	GetInteraction(ctx context.Context, id string) (*pluginsdk.Interaction, error)
	ListWorkflowSteps(ctx context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error)

	Capability(ctx context.Context, workspaceID string) (*pluginsdk.CapabilityContext, error)
	ExactSessions(ctx context.Context, workspaceID, taskID string) ([]pluginsdk.ExactSessionObservation, error)
	ExactInteraction(ctx context.Context, workspaceID, interactionID string) (pluginsdk.ExactInteractionObservation, error)

	StopTaskRun(ctx context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error)
	CancelTurn(ctx context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error)
	GetStepApproval(ctx context.Context, q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error)
	ApproveStep(ctx context.Context, cmd pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error)
	RespondPermission(ctx context.Context, r pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error)
	LinkExternalIdentity(ctx context.Context, r pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error)
	RequestHumanResponseReceipt(ctx context.Context, r pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error)
}

// IsNotFound reports a gRPC NotFound.
func IsNotFound(err error) bool { return status.Code(err) == codes.NotFound }

// IsPermissionDenied reports a gRPC PermissionDenied (missing capability or
// workspace grant).
func IsPermissionDenied(err error) bool { return status.Code(err) == codes.PermissionDenied }

// IsUnimplemented reports a gRPC Unimplemented (older host, Office stage).
func IsUnimplemented(err error) bool {
	return errors.Is(err, ErrUnsupported) || status.Code(err) == codes.Unimplemented
}

const pageSize = 200

// SDK implements Host over a pluginsdk.Host.
type SDK struct{ H pluginsdk.Host }

var _ Host = SDK{}

func (s SDK) GetTask(ctx context.Context, taskID string) (*pluginsdk.Task, error) {
	return s.H.Tasks().Get(ctx, taskID)
}

func (s SDK) ListTasks(ctx context.Context, workspaceID string, includeEphemeral bool) ([]pluginsdk.Task, error) {
	filter := pluginsdk.TaskFilter{WorkspaceIDs: []string{workspaceID}, IncludeArchived: true, IncludeEphemeral: includeEphemeral}
	var out []pluginsdk.Task
	page := pluginsdk.Page{Limit: pageSize}
	for {
		items, info, err := s.H.Tasks().List(ctx, filter, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			return out, nil
		}
		page.Cursor = info.NextCursor
	}
}

func (s SDK) ListSessions(ctx context.Context, taskID string) ([]pluginsdk.Session, error) {
	var out []pluginsdk.Session
	page := pluginsdk.Page{Limit: pageSize}
	for {
		items, info, err := s.H.Sessions().List(ctx, pluginsdk.SessionFilter{TaskIDs: []string{taskID}}, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			return out, nil
		}
		page.Cursor = info.NextCursor
	}
}

func (s SDK) ListMessages(ctx context.Context, taskID, since string) ([]pluginsdk.Message, error) {
	filter := pluginsdk.MessageFilter{TaskIDs: []string{taskID}}
	if since != "" {
		filter.Since = &since
	}
	var out []pluginsdk.Message
	page := pluginsdk.Page{Limit: pageSize}
	for {
		items, info, err := s.H.Messages().List(ctx, filter, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			return out, nil
		}
		page.Cursor = info.NextCursor
	}
}

func (s SDK) SendMessage(ctx context.Context, taskID, text string) (*pluginsdk.MessageDispatch, error) {
	return s.H.Messages().Send(ctx, taskID, "", text)
}

func (s SDK) interactions() (pluginsdk.InteractionAccessor, error) {
	a, ok := pluginsdk.Interactions(s.H)
	if !ok {
		return nil, ErrUnsupported
	}
	return a, nil
}

func (s SDK) ListPendingInteractions(ctx context.Context, taskID string) ([]pluginsdk.Interaction, error) {
	a, err := s.interactions()
	if err != nil {
		return nil, err
	}
	var out []pluginsdk.Interaction
	page := pluginsdk.Page{Limit: pageSize}
	for {
		items, info, err := a.ListPending(ctx, pluginsdk.InteractionFilter{TaskIDs: []string{taskID}}, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			return out, nil
		}
		page.Cursor = info.NextCursor
	}
}

func (s SDK) GetInteraction(ctx context.Context, id string) (*pluginsdk.Interaction, error) {
	a, err := s.interactions()
	if err != nil {
		return nil, err
	}
	return a.Get(ctx, id)
}

func (s SDK) ListWorkflowSteps(ctx context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	return s.H.Workflows().ListSteps(ctx, workflowID)
}

func (s SDK) Capability(ctx context.Context, workspaceID string) (*pluginsdk.CapabilityContext, error) {
	exact, ok := pluginsdk.HostV2(s.H)
	if !ok {
		return nil, ErrUnsupported
	}
	return exact.GetCapabilityContext(ctx, workspaceID)
}

func (s SDK) queries() (pluginsdk.ExactQueryManager, error) {
	q, ok := pluginsdk.HostExactQueries(s.H)
	if !ok {
		return nil, ErrUnsupported
	}
	return q, nil
}

func (s SDK) ExactSessions(ctx context.Context, workspaceID, taskID string) ([]pluginsdk.ExactSessionObservation, error) {
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	var out []pluginsdk.ExactSessionObservation
	page := pluginsdk.ExactReadPage{Limit: pageSize}
	for {
		res, err := q.ListSessions(ctx, pluginsdk.ExactSessionQuery{
			RequestID: newRequestID(), WorkspaceID: workspaceID,
			Filter: pluginsdk.SessionFilter{TaskIDs: []string{taskID}}, Page: page,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if !res.PageInfo.HasMore || res.PageInfo.NextCursor == "" {
			return out, nil
		}
		page.Cursor, page.SnapshotVersion = res.PageInfo.NextCursor, res.PageInfo.SnapshotVersion
	}
}

func (s SDK) ExactInteraction(ctx context.Context, workspaceID, interactionID string) (pluginsdk.ExactInteractionObservation, error) {
	q, err := s.queries()
	if err != nil {
		return pluginsdk.ExactInteractionObservation{}, err
	}
	obs, _, err := q.GetInteraction(ctx, pluginsdk.ExactInteractionGetQuery{
		RequestID: newRequestID(), WorkspaceID: workspaceID, InteractionID: interactionID,
	})
	return obs, err
}

func (s SDK) execution() (pluginsdk.ExactExecutionCommandManager, error) {
	m, ok := pluginsdk.HostExecutionCommands(s.H)
	if !ok {
		return nil, ErrUnsupported
	}
	return m, nil
}

func (s SDK) StopTaskRun(ctx context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
	m, err := s.execution()
	if err != nil {
		return nil, false, err
	}
	return m.StopTaskRun(ctx, cmd)
}

func (s SDK) CancelTurn(ctx context.Context, cmd pluginsdk.ExactTaskExecutionCommand) (*pluginsdk.CommandResult, bool, error) {
	m, err := s.execution()
	if err != nil {
		return nil, false, err
	}
	return m.CancelTurn(ctx, cmd)
}

func (s SDK) GetStepApproval(ctx context.Context, q pluginsdk.ExactStepApprovalQuery) (pluginsdk.StepApproval, error) {
	m, err := s.execution()
	if err != nil {
		return pluginsdk.StepApproval{}, err
	}
	return m.GetStepApproval(ctx, q)
}

func (s SDK) ApproveStep(ctx context.Context, cmd pluginsdk.ExactStepApprovalCommand) (*pluginsdk.CommandResult, pluginsdk.StepApprovalResult, error) {
	m, err := s.execution()
	if err != nil {
		return nil, pluginsdk.StepApprovalResult{}, err
	}
	return m.ApproveStep(ctx, cmd)
}

func (s SDK) RespondPermission(ctx context.Context, r pluginsdk.ExactPermissionResponse) (*pluginsdk.CommandResult, *pluginsdk.Interaction, error) {
	m, ok := pluginsdk.HostExactInteractionCommands(s.H)
	if !ok {
		return nil, nil, ErrUnsupported
	}
	return m.RespondPermission(ctx, r)
}

func (s SDK) delegation() (pluginsdk.HumanResponseDelegationManager, error) {
	m, ok := pluginsdk.HostHumanResponseDelegation(s.H)
	if !ok {
		return nil, ErrUnsupported
	}
	return m, nil
}

func (s SDK) LinkExternalIdentity(ctx context.Context, r pluginsdk.ExternalIdentityLinkRequest) (*pluginsdk.CommandResult, pluginsdk.ExternalIdentityLink, error) {
	m, err := s.delegation()
	if err != nil {
		return nil, pluginsdk.ExternalIdentityLink{}, err
	}
	return m.LinkExternalIdentity(ctx, r)
}

func (s SDK) RequestHumanResponseReceipt(ctx context.Context, r pluginsdk.HumanResponseReceiptRequest) (*pluginsdk.CommandResult, pluginsdk.HumanResponseReceipt, error) {
	m, err := s.delegation()
	if err != nil {
		return nil, pluginsdk.HumanResponseReceipt{}, err
	}
	return m.RequestHumanResponseReceipt(ctx, r)
}
