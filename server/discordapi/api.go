// Package discordapi is the plugin's only boundary to Discord's REST API.
// API is the narrow interface the rest of the plugin depends on; Client
// implements it with bwmarrin/discordgo and Fake records calls for tests.
//
// Implementations never log or return the bot token.
package discordapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bwmarrin/discordgo"
)

// API is every Discord REST call the plugin makes.
type API interface {
	CurrentUser(ctx context.Context) (*discordgo.User, error)
	CurrentApplication(ctx context.Context) (*discordgo.Application, error)
	CurrentUserGuilds(ctx context.Context) ([]*discordgo.UserGuild, error)

	// GuildMember returns a member with roles (REST; no privileged intent).
	GuildMember(ctx context.Context, guildID, userID string) (*discordgo.Member, error)
	GuildRoles(ctx context.Context, guildID string) ([]*discordgo.Role, error)
	GuildRoleCreate(ctx context.Context, guildID string, params *discordgo.RoleParams) (*discordgo.Role, error)
	GuildChannels(ctx context.Context, guildID string) ([]*discordgo.Channel, error)
	GuildChannelCreate(ctx context.Context, guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	ChannelEdit(ctx context.Context, channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error)
	ChannelDelete(ctx context.Context, channelID string) error

	// ChannelMessages returns up to limit messages newer than afterID, in
	// the order Discord returns them (newest first).
	ChannelMessages(ctx context.Context, channelID string, limit int, afterID string) ([]*discordgo.Message, error)
	MessageSend(ctx context.Context, channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)
	MessageEdit(ctx context.Context, data *discordgo.MessageEdit) (*discordgo.Message, error)
	ReactionAdd(ctx context.Context, channelID, messageID, emoji string) error
	MessagePin(ctx context.Context, channelID, messageID string) error

	BulkOverwriteGuildCommands(ctx context.Context, appID, guildID string, commands []*discordgo.ApplicationCommand) error
	InteractionRespond(ctx context.Context, interaction *discordgo.Interaction, resp *discordgo.InteractionResponse) error
	InteractionEdit(ctx context.Context, interaction *discordgo.Interaction, edit *discordgo.WebhookEdit) error
}

// Discord JSON error codes the plugin branches on.
const (
	CodeUnknownChannel          = 10003
	CodeUnknownMessage          = 10008
	CodeUnknownInteraction      = 10062
	CodeMaxGuildChannels        = 30013
	CodeMissingAccess           = 50001
	CodeMissingPermissions      = 50013
	CodeMaxPinsReached          = 30003
	CodeInteractionAcknowledged = 40060
)

// ErrHalted is returned for every bot call after Discord rejected the token
// with HTTP 401, until the plugin restarts with new settings.
var ErrHalted = errors.New("discord calls are halted: the bot token was rejected (HTTP 401)")

// Error is a sanitized non-2xx Discord response.
type Error struct {
	Status  int
	Code    int
	Message string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("discord API returned HTTP %d", e.Status)
	if e.Code != 0 {
		msg += fmt.Sprintf(" (code %d)", e.Code)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Status returns the HTTP status of a Discord error, or 0.
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// HasCode reports whether err is a Discord error with the JSON code.
func HasCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// IsGone reports whether the target channel or message no longer exists or
// is no longer reachable by the bot.
func IsGone(err error) bool {
	return Status(err) == http.StatusNotFound || HasCode(err, CodeUnknownChannel) || HasCode(err, CodeUnknownMessage)
}

// IsForbidden reports a 403 (missing access or permissions).
func IsForbidden(err error) bool { return Status(err) == http.StatusForbidden }

// NoMentions is the allowed-mentions value every bot message uses, so agent
// text can never ping @everyone, roles, or users.
func NoMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}
