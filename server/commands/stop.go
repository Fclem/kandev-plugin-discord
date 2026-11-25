package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/outbound"
)

// Replies shared by commands that need a Host v2 grant.
const (
	msgExecutionGrant = "A Kandev workspace manager must grant execution control to the Discord plugin (Settings > Plugins > Workspace access: `host.v2.write:execution` and `host.v2.read:sessions`)."
	msgUnsupported    = "This Kandev version does not support that from Discord yet."
)

// stop cancels the current turn (scope turn) or stops each session's run
// (scope task), as the operator, through the exact execution commands.
func (c *call) stop(ctx context.Context, taskID, scope string) {
	if !c.requireOperator(ctx) {
		return
	}
	ws := c.binding.WorkspaceID
	sessions, err := c.Host.ExactSessions(ctx, ws, taskID)
	if err != nil {
		c.reply(ctx, hostErrorText(err, msgExecutionGrant))
		return
	}
	var targets []pluginsdk.ExactSessionObservation
	for _, s := range sessions {
		if s.ExecutionID == "" {
			continue
		}
		if scope == ScopeTask || outbound.Running(s.Session.State) {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		c.reply(ctx, "Nothing is running.")
		return
	}
	capability, err := c.Host.Capability(ctx, ws)
	if err != nil {
		c.reply(ctx, hostErrorText(err, msgExecutionGrant))
		return
	}
	var lines []string
	for _, s := range targets {
		cmd := pluginsdk.ExactTaskExecutionCommand{
			RequestID: kandev.NewRequestID(), WorkspaceID: ws, TaskID: taskID, SessionID: s.Session.ID,
			ExpectedSessionResourceVersion: s.ResourceVersion, ExpectedExecutionID: s.ExecutionID,
			IdempotencyKey: "discord:stop:" + scope + ":" + c.i.ID + ":" + s.Session.ID,
			// An unclaimed task: no manager instance, generation zero.
			ApprovalRevision: capability.ApprovalRevision, ManifestDigest: capability.ManifestDigest,
		}
		var result *pluginsdk.CommandResult
		if scope == ScopeTask {
			result, _, err = c.Host.StopTaskRun(ctx, cmd)
		} else {
			result, _, err = c.Host.CancelTurn(ctx, cmd)
		}
		lines = append(lines, stopOutcome(sessionName(s.Session), scope, result, err))
	}
	c.reply(ctx, strings.Join(lines, "\n"))
}

func sessionName(s pluginsdk.Session) string {
	for _, n := range []string{s.AgentDisplayName, s.AgentProfileName} {
		if n != "" {
			return n
		}
	}
	return "the agent"
}

func stopOutcome(name, scope string, result *pluginsdk.CommandResult, err error) string {
	if err != nil {
		return hostErrorText(err, msgExecutionGrant)
	}
	if result == nil {
		return "Kandev returned no result; check the task in Kandev."
	}
	switch result.Status {
	case pluginsdk.CommandApplied, pluginsdk.CommandAlreadyApplied:
		if scope == ScopeTask {
			return fmt.Sprintf("⏹️ Stopped the run of %s.", name)
		}
		return fmt.Sprintf("⏹️ Cancelled the current turn of %s; the session stays open.", name)
	case pluginsdk.CommandNoChange:
		return "Nothing is running."
	case pluginsdk.CommandConflict:
		return "The session changed (or the task is managed elsewhere); run /stop again."
	case pluginsdk.CommandDenied:
		return msgExecutionGrant
	case pluginsdk.CommandUnsupported:
		return msgUnsupported
	}
	return fmt.Sprintf("Kandev could not do that right now (%s); try again.", strings.ToLower(string(result.Status)))
}

// hostErrorText maps a Host RPC error to a reply.
func hostErrorText(err error, grant string) string {
	switch {
	case kandev.IsPermissionDenied(err):
		return grant
	case kandev.IsUnimplemented(err):
		return msgUnsupported
	case kandev.IsNotFound(err):
		return "Kandev no longer has that task or request."
	}
	return "Kandev is unavailable right now; try again."
}
