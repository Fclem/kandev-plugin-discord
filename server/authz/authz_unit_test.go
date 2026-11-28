package authz

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"

	"kandev-plugin-discord/server/config"
)

func TestUserIDPrefersTheGuildMember(t *testing.T) {
	member := &discordgo.Member{User: &discordgo.User{ID: "member"}}
	dm := &discordgo.User{ID: "user"}
	assert.Equal(t, "member", UserID(member, dm), "guild interactions carry the member")
	assert.Equal(t, "user", UserID(&discordgo.Member{}, dm), "a member without user falls back")
	assert.Equal(t, "user", UserID(nil, dm))
	assert.Empty(t, UserID(nil, nil))
}

func TestDisplayNameFallbacks(t *testing.T) {
	assert.Equal(t, "someone", DisplayName(nil, nil))
	assert.Equal(t, "someone", DisplayName(&discordgo.Member{}, nil))
	assert.Equal(t, "Bobby", DisplayName(nil, &discordgo.User{Username: "bob", GlobalName: "Bobby"}))
	assert.Equal(t, "carol", DisplayName(&discordgo.Member{User: &discordgo.User{Username: "carol"}}, &discordgo.User{Username: "other"}),
		"the member's user wins over the interaction user")
}

func TestAllowedUserNeedsNoMemberOrRole(t *testing.T) {
	cfg := config.Config{AllowedUserIDs: []string{"111111111111111111"}}
	assert.True(t, IsOperator(cfg, "role-op", &discordgo.Member{Roles: []string{"role-op"}}, ""),
		"the operator role counts without a user ID")
	assert.False(t, IsOperator(config.Config{}, "role-op", &discordgo.Member{}, "111111111111111111"),
		"no allow list and no role")
}
