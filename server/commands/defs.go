// Package commands handles Discord interactions: the guild slash commands,
// approval buttons, and autocomplete.
package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/bwmarrin/discordgo"
)

// Command names.
const (
	CmdLink    = "link"
	CmdStatus  = "status"
	CmdStop    = "stop"
	CmdApprove = "approve"
	CmdDeny    = "deny"

	OptCode    = "code"
	OptScope   = "scope"
	OptRequest = "request"

	ScopeTurn = "turn"
	ScopeTask = "task"
)

// Definitions are the guild-scoped commands the plugin registers.
func Definitions() []*discordgo.ApplicationCommand {
	request := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: OptRequest,
		Description: "Pending request (defaults to the only one)", Autocomplete: true,
	}
	return []*discordgo.ApplicationCommand{
		{
			Name: CmdLink, Description: "Link your Discord account to your Kandev user with a code from Kandev",
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: OptCode, Required: true,
				Description: "Link code from Settings > Plugins > Discord > Linked chat accounts",
			}},
		},
		{Name: CmdStatus, Description: "Show this task's state, sessions, queue, and pending requests"},
		{
			Name: CmdStop, Description: "Stop the agent in this task channel",
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: OptScope, Description: "What to stop (default: turn)",
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "current turn (session stays open)", Value: ScopeTurn},
					{Name: "whole task run", Value: ScopeTask},
				},
			}},
		},
		{Name: CmdApprove, Description: "Approve a pending permission request or workflow step", Options: []*discordgo.ApplicationCommandOption{request}},
		{Name: CmdDeny, Description: "Deny a pending permission request", Options: []*discordgo.ApplicationCommandOption{request}},
	}
}

// DefinitionsHash identifies the registered command set for one application,
// so provisioning re-registers only on change.
func DefinitionsHash(applicationID string) string {
	raw, _ := json.Marshal(Definitions())
	sum := sha256.Sum256(append([]byte(applicationID+"\x00"), raw...))
	return hex.EncodeToString(sum[:])
}
