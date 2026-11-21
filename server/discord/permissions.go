package discord

import (
	"net/url"
	"strconv"
)

// Permission is one Discord permission bit.
type Permission struct {
	Name string
	Bit  uint64
}

// Administrator bypasses every other permission check. The invite never asks
// for it, but a server owner may grant it.
const Administrator uint64 = 1 << 3

// RequiredPermissions are the bot permissions the plugin needs in a bound
// server: provisioning roles and categories, one channel per task, posting,
// reacting, and pinning the task card.
var RequiredPermissions = []Permission{
	{"MANAGE_CHANNELS", 1 << 4},
	{"ADD_REACTIONS", 1 << 6},
	{"VIEW_CHANNEL", 1 << 10},
	{"SEND_MESSAGES", 1 << 11},
	{"EMBED_LINKS", 1 << 14},
	{"READ_MESSAGE_HISTORY", 1 << 16},
	{"MANAGE_ROLES", 1 << 28},
	{"PIN_MESSAGES", 1 << 51},
}

// Application flags reporting the Message Content privileged intent:
// GATEWAY_MESSAGE_CONTENT (approved, 100+ servers) and
// GATEWAY_MESSAGE_CONTENT_LIMITED (Bot page toggle, under 100 servers).
const (
	FlagGatewayMessageContent        uint64 = 1 << 18
	FlagGatewayMessageContentLimited uint64 = 1 << 19
)

// MessageContentIntentEnabled reports whether the application may receive
// message content over the gateway.
func MessageContentIntentEnabled(flags uint64) bool {
	return flags&(FlagGatewayMessageContent|FlagGatewayMessageContentLimited) != 0
}

// RequiredPermissionBits is the OR of RequiredPermissions.
func RequiredPermissionBits() uint64 {
	var bits uint64
	for _, p := range RequiredPermissions {
		bits |= p.Bit
	}
	return bits
}

// MissingPermissions returns the names of required permissions absent from
// granted. Administrator grants everything.
func MissingPermissions(granted uint64) []string {
	if granted&Administrator != 0 {
		return nil
	}
	var missing []string
	for _, p := range RequiredPermissions {
		if granted&p.Bit == 0 {
			missing = append(missing, p.Name)
		}
	}
	return missing
}

// InviteURL builds the OAuth2 bot invite for applicationID with the required
// permissions, preselecting guildID when non-empty.
func InviteURL(applicationID, guildID string) string {
	q := url.Values{}
	q.Set("client_id", applicationID)
	q.Set("scope", "bot applications.commands")
	q.Set("permissions", strconv.FormatUint(RequiredPermissionBits(), 10))
	if guildID != "" {
		q.Set("guild_id", guildID)
		q.Set("disable_guild_select", "true")
	}
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}
