// Package authz decides who may act on Kandev from Discord. Roles are read
// from each gateway payload (or a fresh member read) on every action.
package authz

import (
	"slices"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/config"
)

// IsOperator reports whether a member holds the operator role or is listed
// in allowed_user_ids.
func IsOperator(cfg config.Config, operatorRoleID string, member *discordgo.Member, userID string) bool {
	if userID != "" && cfg.IsAllowedUser(userID) {
		return true
	}
	return member != nil && operatorRoleID != "" && slices.Contains(member.Roles, operatorRoleID)
}

// UserID returns the acting user of an interaction or message member.
func UserID(member *discordgo.Member, user *discordgo.User) string {
	if member != nil && member.User != nil {
		return member.User.ID
	}
	if user != nil {
		return user.ID
	}
	return ""
}

// DisplayName is the name Discord shows for a member: nickname, global
// name, then username.
func DisplayName(member *discordgo.Member, user *discordgo.User) string {
	if member != nil && member.Nick != "" {
		return member.Nick
	}
	if member != nil && member.User != nil {
		user = member.User
	}
	if user == nil {
		return "someone"
	}
	if user.GlobalName != "" {
		return user.GlobalName
	}
	return user.Username
}
