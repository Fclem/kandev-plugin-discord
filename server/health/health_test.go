package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
)

const (
	appID = "123456789012345678"
	guild = "223456789012345678"
	token = "MTIz.secret-token-value"
)

type fakeAPI struct {
	user      *discordgo.User
	userErr   error
	app       *discordgo.Application
	appErr    error
	guilds    []*discordgo.UserGuild
	guildsErr error
}

func (f fakeAPI) CurrentUser(context.Context) (*discordgo.User, error) { return f.user, f.userErr }
func (f fakeAPI) CurrentApplication(context.Context) (*discordgo.Application, error) {
	return f.app, f.appErr
}
func (f fakeAPI) CurrentUserGuilds(context.Context) ([]*discordgo.UserGuild, error) {
	return f.guilds, f.guildsErr
}

func healthyAPI() fakeAPI {
	return fakeAPI{
		user:   &discordgo.User{ID: "1", Username: "kandev-bot", Bot: true},
		app:    &discordgo.Application{ID: appID, Name: "Kandev", Flags: int(discordapi.FlagGatewayMessageContentLimited)},
		guilds: []*discordgo.UserGuild{{ID: guild, Name: "Team", Permissions: int64(discordapi.RequiredPermissionBits())}},
	}
}

func validConfig() config.Config {
	return config.Config{BotToken: token, ApplicationID: appID, Bindings: []config.Binding{{WorkspaceID: "ws", GuildID: guild}}}
}

func statuses(r Result) map[string]string {
	out := map[string]string{}
	for _, c := range r.Checks {
		out[c.ID] = c.Status
	}
	return out
}

func TestAllChecksPass(t *testing.T) {
	r := Test(context.Background(), healthyAPI(), validConfig(), nil, "ws")
	assert.True(t, r.OK)
	assert.Empty(t, r.InviteURL)
	assert.Equal(t, map[string]string{
		CheckConfig: Pass, CheckToken: Pass, CheckApplication: Pass,
		CheckIntent: Pass, CheckMembership: Pass, CheckPermissions: Pass,
	}, statuses(r))
}

func TestRejectedTokenSkipsDependentChecks(t *testing.T) {
	api := healthyAPI()
	api.userErr = &discordapi.Error{Status: http.StatusUnauthorized, Message: "401: Unauthorized"}
	r := Test(context.Background(), api, validConfig(), nil, "ws")
	assert.False(t, r.OK)
	s := statuses(r)
	assert.Equal(t, Fail, s[CheckToken])
	assert.Equal(t, Skip, s[CheckMembership])
	assert.Contains(t, r.Checks[1].Detail, "rejected the bot token")
}

func TestBotNotInServerReturnsInvite(t *testing.T) {
	api := healthyAPI()
	api.guilds = nil
	r := Test(context.Background(), api, validConfig(), nil, "ws")
	assert.False(t, r.OK)
	assert.Equal(t, Fail, statuses(r)[CheckMembership])
	assert.Contains(t, r.InviteURL, "guild_id="+guild)
}

func TestMissingPermissionsAndIntent(t *testing.T) {
	api := healthyAPI()
	api.app.Flags = 0
	api.guilds[0].Permissions = int64(discordapi.RequiredPermissionBits() &^ (1 << 28))
	r := Test(context.Background(), api, validConfig(), nil, "ws")
	s := statuses(r)
	assert.Equal(t, Fail, s[CheckIntent])
	assert.Equal(t, Fail, s[CheckPermissions])
	assert.NotEmpty(t, r.InviteURL)
}

func TestApplicationMismatch(t *testing.T) {
	api := healthyAPI()
	api.app.ID = "999999999999999999"
	r := Test(context.Background(), api, validConfig(), nil, "ws")
	assert.Equal(t, Fail, statuses(r)[CheckApplication])
}

func TestUnboundWorkspaceStillChecksToken(t *testing.T) {
	r := Test(context.Background(), healthyAPI(), validConfig(), nil, "other-ws")
	s := statuses(r)
	assert.Equal(t, Fail, s[CheckConfig])
	assert.Equal(t, Pass, s[CheckToken])
	assert.Equal(t, Skip, s[CheckMembership])
}

func TestMissingTokenMakesNoCalls(t *testing.T) {
	api := fakeAPI{userErr: errors.New("must not be called")}
	r := Test(context.Background(), api, config.Config{}, errors.New("bot_token is required"), "ws")
	s := statuses(r)
	assert.Equal(t, Fail, s[CheckConfig])
	assert.Equal(t, Skip, s[CheckToken])
}

func TestResultNeverContainsToken(t *testing.T) {
	api := healthyAPI()
	api.guildsErr = errors.New("boom")
	r := Test(context.Background(), api, validConfig(), nil, "ws")
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), token)
}
