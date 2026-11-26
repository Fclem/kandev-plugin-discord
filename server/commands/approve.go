package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/outbound"
	"kandev-plugin-discord/server/prompts"
	"kandev-plugin-discord/server/store"
)

// Request values offered by autocomplete.
const (
	requestPermission = "perm:"
	requestStep       = "step:"
)

const msgNothingAwaiting = "Nothing is awaiting approval."

// approve handles /approve and /deny. With no request, a single pending
// permission request wins over the workflow step; /deny never applies to
// steps.
func (c *call) approve(ctx context.Context, taskID, request string, allow bool) {
	if !c.requireOperator(ctx) {
		return
	}
	pick := pickOption(allow)
	switch {
	case strings.HasPrefix(request, requestPermission):
		c.respondPermission(ctx, taskID, strings.TrimPrefix(request, requestPermission), pick)
		return
	case strings.HasPrefix(request, requestStep) && allow:
		session, stepKey, _ := strings.Cut(strings.TrimPrefix(request, requestStep), ":")
		c.approveStep(ctx, taskID, session, stepKey, "")
		return
	case request != "":
		c.reply(ctx, "Pick a request from the list.")
		return
	}
	pending, err := c.pendingPermissions(ctx, taskID)
	if err != nil {
		c.reply(ctx, hostErrorText(err, msgApprovalGrant))
		return
	}
	switch {
	case len(pending) == 1:
		c.respondPermission(ctx, taskID, pending[0].ID, pick)
	case len(pending) > 1:
		c.reply(ctx, fmt.Sprintf("%d permission requests are pending; choose one with the `request` option.", len(pending)))
	case !allow:
		c.reply(ctx, "There is no permission request to deny. To request changes on a workflow step, post a message in this channel.")
	default:
		session, err := c.awaitingSession(ctx, taskID)
		if err != nil {
			c.reply(ctx, c.stepErrorText(err, taskID))
			return
		}
		if session == "" {
			c.reply(ctx, msgNothingAwaiting)
			return
		}
		c.approveStep(ctx, taskID, session, "", "")
	}
}

func (c *call) pendingPermissions(ctx context.Context, taskID string) ([]pluginsdk.Interaction, error) {
	all, err := c.Host.ListPendingInteractions(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var out []pluginsdk.Interaction
	for _, it := range all {
		if it.Kind == pluginsdk.InteractionKindPermission {
			out = append(out, it)
		}
	}
	return out, nil
}

// awaitingSession returns the session whose step awaits approval, or "".
func (c *call) awaitingSession(ctx context.Context, taskID string) (string, error) {
	sessions, err := c.Host.ListSessions(ctx, taskID)
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		if outbound.Running(s.State) {
			continue
		}
		a, err := c.Host.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{
			RequestID: kandev.NewRequestID(), WorkspaceID: c.binding.WorkspaceID, SessionID: s.ID,
		})
		if err != nil {
			return "", err
		}
		if a.Awaiting {
			return s.ID, nil
		}
	}
	return "", nil
}

// pickOption chooses /approve's or /deny's option: the "once" kind first,
// then any allow_* or reject_* option. A button passes its own index.
func pickOption(allow bool) func([]pluginsdk.InteractionOption) int {
	prefix, preferred := "reject", "reject_once"
	if allow {
		prefix, preferred = "allow", "allow_once"
	}
	return func(opts []pluginsdk.InteractionOption) int {
		for i, o := range opts {
			if o.Kind == preferred {
				return i
			}
		}
		for i, o := range opts {
			if strings.HasPrefix(o.Kind, prefix) {
				return i
			}
		}
		return -1
	}
}

// capability returns the approval revision and manifest digest, or a reply
// when the workspace has granted nothing.
func (c *call) capability(ctx context.Context) (*pluginsdk.CapabilityContext, string) {
	capability, err := c.Host.Capability(ctx, c.binding.WorkspaceID)
	if err != nil {
		return nil, hostErrorText(err, msgApprovalGrant)
	}
	if capability.ApprovalRevision == 0 {
		return nil, msgApprovalGrant
	}
	return capability, ""
}

// respondPermission answers one permission request as the caller's linked
// Kandev user: exact read, receipt, exact response.
func (c *call) respondPermission(ctx context.Context, taskID, interactionID string, pick func([]pluginsdk.InteractionOption) int) {
	capability, refusal := c.capability(ctx)
	if refusal != "" {
		c.reply(ctx, refusal)
		return
	}
	ws := c.binding.WorkspaceID
	obs, err := c.Host.ExactInteraction(ctx, ws, interactionID)
	if err != nil {
		if kandev.IsNotFound(err) {
			c.reply(ctx, "That request no longer exists in Kandev.")
			c.markPrompt(taskID, interactionID, "⏹️ No longer pending.")
			return
		}
		c.reply(ctx, hostErrorText(err, msgApprovalGrant))
		return
	}
	it := obs.Interaction
	if it.TaskID != taskID {
		c.reply(ctx, "That request belongs to another task.")
		return
	}
	if it.Status != pluginsdk.InteractionStatusPending {
		c.reply(ctx, "That request was already resolved.")
		c.markPrompt(taskID, interactionID, prompts.ResolvedText(&it, nil))
		return
	}
	index := pick(it.Options)
	if index < 0 || index >= len(it.Options) {
		c.reply(ctx, "This request has no matching option here; answer it in Kandev"+c.linkSuffix(taskID))
		return
	}
	option := it.Options[index]
	result, receipt, err := c.Host.RequestHumanResponseReceipt(ctx, pluginsdk.HumanResponseReceiptRequest{
		RequestID: kandev.NewRequestID(), WorkspaceID: ws, Provider: Provider, Subject: c.userID,
		Kind: pluginsdk.HumanResponseKindPermission, TargetID: it.ID, ExpectedResourceVersion: obs.ResourceVersion,
		OptionID: option.OptionID, ApprovalRevision: capability.ApprovalRevision, ManifestDigest: capability.ManifestDigest,
	})
	if text, resolved := c.refusal(taskID, result, err); text != "" {
		c.reply(ctx, text)
		if resolved {
			c.markPrompt(taskID, interactionID, "✅ Resolved elsewhere.")
		}
		return
	}
	result, answered, err := c.Host.RespondPermission(ctx, pluginsdk.ExactPermissionResponse{
		RequestID: kandev.NewRequestID(), WorkspaceID: ws, InteractionID: it.ID, ExpectedResourceVersion: obs.ResourceVersion,
		OptionID: option.OptionID, HumanResponseReceiptID: receipt.ID,
		ApprovalRevision: capability.ApprovalRevision, ManifestDigest: capability.ManifestDigest,
	})
	if text, resolved := c.refusal(taskID, result, err); text != "" {
		c.reply(ctx, text)
		if resolved {
			c.markPrompt(taskID, interactionID, prompts.ResolvedText(answered, nil))
		}
		return
	}
	actor := receipt.Actor
	if answered != nil && answered.ResolvedBy != nil {
		actor = *answered.ResolvedBy
	}
	c.reply(ctx, fmt.Sprintf("✅ %s — recorded in Kandev as %s via Discord.", option.Label, actor.DisplayName))
	c.markPrompt(taskID, interactionID, fmt.Sprintf("✅ %s by %s via %s.", option.Label, actor.DisplayName, viaName(actor)))
}

// approveStep approves the session's awaiting workflow step as the
// caller's linked Kandev user, like the native Approve button. A stepKey
// (from a prompt button or autocomplete) must still name the awaiting
// transition, and a prompt nonce must name the prompt currently posted for
// the current approval state (its ResourceVersion); otherwise the prompt
// is stale and nothing is approved.
func (c *call) approveStep(ctx context.Context, taskID, sessionID, stepKey, nonce string) {
	capability, refusal := c.capability(ctx)
	if refusal != "" {
		c.reply(ctx, refusal)
		return
	}
	ws := c.binding.WorkspaceID
	approval, err := c.Host.GetStepApproval(ctx, pluginsdk.ExactStepApprovalQuery{RequestID: kandev.NewRequestID(), WorkspaceID: ws, SessionID: sessionID})
	if err != nil {
		c.reply(ctx, c.stepErrorText(err, taskID))
		return
	}
	if approval.TaskID != "" && approval.TaskID != taskID {
		c.reply(ctx, "That step belongs to another task.")
		return
	}
	if !approval.Awaiting {
		c.reply(ctx, msgNothingAwaiting)
		c.Refresh(taskID)
		return
	}
	stale := stepKey != "" && prompts.StepKey(approval.FromStepID, approval.ToStepID) != stepKey
	if nonce != "" && !stale {
		posted, found, err := c.Store.StepApproval(ctx, taskID)
		if err != nil {
			c.reply(ctx, "Kandev could not check this prompt right now; try again.")
			return
		}
		stale = !found || posted.SessionID != sessionID || posted.Nonce != nonce || posted.ResourceVersion != approval.ResourceVersion
	}
	if stale {
		c.reply(ctx, "That approval prompt is out of date. Nothing was approved; use the current prompt.")
		c.Refresh(taskID)
		return
	}
	result, receipt, err := c.Host.RequestHumanResponseReceipt(ctx, pluginsdk.HumanResponseReceiptRequest{
		RequestID: kandev.NewRequestID(), WorkspaceID: ws, Provider: Provider, Subject: c.userID,
		Kind: pluginsdk.HumanResponseKindStepApproval, TargetID: sessionID, ExpectedResourceVersion: approval.ResourceVersion,
		ApprovalRevision: capability.ApprovalRevision, ManifestDigest: capability.ManifestDigest,
	})
	if text, _ := c.refusal(taskID, result, err); text != "" {
		c.reply(ctx, text)
		return
	}
	result, _, err = c.Host.ApproveStep(ctx, pluginsdk.ExactStepApprovalCommand{
		RequestID: kandev.NewRequestID(), WorkspaceID: ws, SessionID: sessionID,
		ExpectedResourceVersion: approval.ResourceVersion, IdempotencyKey: "discord:approve:" + c.i.ID,
		HumanResponseReceiptID: receipt.ID, ApprovalRevision: capability.ApprovalRevision, ManifestDigest: capability.ManifestDigest,
	})
	if text, _ := c.refusal(taskID, result, err); text != "" {
		c.reply(ctx, text)
		return
	}
	moved := "Step **" + approval.FromStepName + "** approved"
	if approval.ToStepName != "" {
		moved += " → **" + approval.ToStepName + "**"
	}
	c.reply(ctx, fmt.Sprintf("✅ %s, recorded in Kandev as %s via Discord.", moved, receipt.Actor.DisplayName))
	approval.SessionID = sessionID
	c.markStep(taskID, approval, fmt.Sprintf("✅ Approved by %s via %s.", receipt.Actor.DisplayName, viaName(receipt.Actor)))
}

// refusal maps a receipt or command outcome to a reply; resolved reports
// that the target was already answered elsewhere.
func (c *call) refusal(taskID string, result *pluginsdk.CommandResult, err error) (string, bool) {
	if err != nil {
		if kandev.IsUnimplemented(err) {
			return "This Kandev version cannot take approvals from Discord; answer in Kandev" + c.linkSuffix(taskID), false
		}
		return hostErrorText(err, msgApprovalGrant), false
	}
	if result == nil {
		return "Kandev returned no result; try again.", false
	}
	switch result.Status {
	case pluginsdk.CommandApplied, pluginsdk.CommandAlreadyApplied:
		return "", false
	case pluginsdk.CommandDenied:
		switch {
		case result.Reason == "external_identity_not_linked":
			return "Your Discord account is not linked to a Kandev user, so nothing changed. Answer in Kandev" + c.linkSuffix(taskID) + "\n" + msgLinkHowTo, false
		case result.Reason == "linked_user_not_authorized":
			return "Your linked Kandev account cannot control sessions in this workspace, so nothing changed.", false
		case result.Reason == "stale_capability_revision":
			return "Kandev's access settings just changed; try again.", false
		case isGrantReason(result.Reason):
			return msgApprovalGrant, false
		}
		return "Kandev refused the response (" + result.Reason + "); nothing changed.", false
	case pluginsdk.CommandConflict:
		switch result.Reason {
		case "step_not_awaiting_approval":
			return msgNothingAwaiting, false
		case "step_approval_changed":
			return "The task moved before the approval landed; check `/status`.", false
		case "interaction_revision_changed":
			return "The request changed; try again.", false
		}
		return "That request was already resolved.", true
	case pluginsdk.CommandNotFound:
		return "That request was already resolved.", true
	case pluginsdk.CommandUnsupported:
		return "This is an Office approval stage; approve it in Kandev" + c.linkSuffix(taskID), false
	}
	return fmt.Sprintf("Kandev could not do that right now (%s); try again.", strings.ToLower(string(result.Status))), false
}

func (c *call) stepErrorText(err error, taskID string) string {
	if kandev.IsUnimplemented(err) {
		return "This is an Office approval stage; approve it in Kandev" + c.linkSuffix(taskID)
	}
	return hostErrorText(err, msgApprovalGrant)
}

func (c *call) linkSuffix(taskID string) string {
	if url := c.Config.TaskURL(taskID); url != "" {
		return ": " + url
	}
	return "."
}

func viaName(a pluginsdk.InteractionActor) string {
	if a.Via != "" {
		return a.Via
	}
	return Provider
}

func (c *call) poster() *prompts.Poster {
	return &prompts.Poster{API: c.API, Host: c.Host, Store: c.Store, TaskURL: c.Config.TaskURL, Now: time.Now}
}

// markPrompt resolves a posted permission prompt and forgets it, on the
// task's queue. A prompt whose buttons could not be removed stays recorded;
// the sync resolves it later.
func (c *call) markPrompt(taskID, interactionID, outcome string) {
	c.OnTask(taskID, func(ctx context.Context) {
		rec, found, err := c.Store.Channel(ctx, taskID)
		if err != nil || !found {
			return
		}
		stored, err := c.Store.Prompts(ctx, taskID)
		if err != nil {
			return
		}
		kept := stored.Items[:0:0]
		for _, p := range stored.Items {
			if p.InteractionID == interactionID {
				if err := c.poster().Resolve(ctx, rec.ChannelID, p.MessageID, outcome); err == nil {
					continue
				}
			}
			kept = append(kept, p)
		}
		if len(kept) != len(stored.Items) {
			_ = c.Store.PutPrompts(ctx, taskID, store.Prompts{Items: kept})
		}
	})
	c.Refresh(taskID)
}

// markStep resolves the posted step approval prompt of the approved
// transition at the approved state (ResourceVersion) and forgets it; a
// prompt for another transition or a later visit, or one whose buttons
// could not be removed, is left to the sync.
func (c *call) markStep(taskID string, approval pluginsdk.StepApproval, outcome string) {
	c.OnTask(taskID, func(ctx context.Context) {
		rec, found, err := c.Store.Channel(ctx, taskID)
		if err != nil || !found {
			return
		}
		step, posted, err := c.Store.StepApproval(ctx, taskID)
		if err != nil || !posted || step.SessionID != approval.SessionID || step.FromStepID != approval.FromStepID ||
			step.ToStepID != approval.ToStepID || step.ResourceVersion != approval.ResourceVersion {
			return
		}
		if err := c.poster().Resolve(ctx, rec.ChannelID, step.MessageID, outcome); err != nil {
			return
		}
		_ = c.Store.DeleteStepApproval(ctx, taskID)
	})
	c.Refresh(taskID)
}
