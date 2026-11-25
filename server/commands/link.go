package commands

import (
	"context"
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/kandev"
)

// Provider is the external identity provider name the plugin links.
const Provider = "discord"

// Replies about linking and approvals.
const (
	msgApprovalGrant = "A Kandev workspace manager must grant Discord approvals to this plugin first (Settings > Plugins > Workspace access: `host.v2.write:human_responses`, `host.v2.write:interactions`, and `host.v2.read:interactions`; step approvals also need `host.v2.write:execution` and `host.v2.read:sessions`). Nothing changed in Kandev."
	msgLinkHowTo     = "To link: in Kandev open Settings > Plugins > Discord > **Linked chat accounts**, generate a code, then run `/link <code>` here within ten minutes."
)

// link redeems a Kandev link code for the caller's Discord account. The
// reply never echoes the code.
func (c *call) link(ctx context.Context, code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		c.reply(ctx, "Provide the code from Kandev. "+msgLinkHowTo)
		return
	}
	result, link, err := c.Host.LinkExternalIdentity(ctx, pluginsdk.ExternalIdentityLinkRequest{
		RequestID: kandev.NewRequestID(), Code: code, Provider: Provider, Subject: c.userID, DisplayName: c.userName,
	})
	if err != nil {
		c.reply(ctx, hostErrorText(err, msgApprovalGrant))
		return
	}
	switch {
	case result == nil:
		c.reply(ctx, "Kandev returned no result; try again.")
	case result.Status == pluginsdk.CommandApplied || result.Status == pluginsdk.CommandAlreadyApplied:
		who := link.UserDisplayName
		if who == "" {
			who = "your Kandev user"
		}
		c.reply(ctx, "🔗 Linked your Discord account to **"+who+"**. Approvals you make here are recorded in Kandev as "+who+" via Discord. You or an admin can revoke the link on the plugin's settings page.")
	case result.Status == pluginsdk.CommandDenied && isGrantReason(result.Reason):
		c.reply(ctx, "A Kandev workspace manager must grant `host.v2.write:human_responses` to the Discord plugin before accounts can be linked. Your code stays valid until it expires; run `/link` again after the grant.")
	case result.Status == pluginsdk.CommandUnavailable:
		c.reply(ctx, "Kandev could not link the account right now; try again.")
	default:
		c.reply(ctx, "That link code is invalid, expired, or already used. "+msgLinkHowTo)
	}
}

// isGrantReason reports denial reasons caused by the workspace grant.
func isGrantReason(reason string) bool {
	switch reason {
	case "missing_capability_approval", "capability_revoked", "undeclared_capability", "unavailable_capability", "unsupported_capability":
		return true
	}
	return false
}
