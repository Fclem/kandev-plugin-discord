package authz

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"

	"kandev-plugin-discord/server/config"
)

func TestIsOperator(t *testing.T) {
	cfg := config.Config{AllowedUserIDs: []string{"111111111111111111"}}
	op := &discordgo.Member{Roles: []string{"role-op"}}
	other := &discordgo.Member{Roles: []string{"role-x"}}
	assert.True(t, IsOperator(cfg, "role-op", op, "222222222222222222"))
	assert.False(t, IsOperator(cfg, "role-op", other, "222222222222222222"))
	assert.True(t, IsOperator(cfg, "role-op", other, "111111111111111111"), "allowed_user_ids")
	assert.False(t, IsOperator(cfg, "", other, "333333333333333333"), "no role configured yet")
	assert.False(t, IsOperator(cfg, "role-op", nil, "333333333333333333"))
}

func TestDisplayName(t *testing.T) {
	u := &discordgo.User{ID: "1", Username: "alice_1", GlobalName: "Alice"}
	assert.Equal(t, "Ali", DisplayName(&discordgo.Member{Nick: "Ali", User: u}, nil))
	assert.Equal(t, "Alice", DisplayName(&discordgo.Member{User: u}, nil))
	assert.Equal(t, "bob", DisplayName(nil, &discordgo.User{Username: "bob"}))
}
