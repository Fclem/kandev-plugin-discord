package main

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/discord"
	"kandev-plugin-discord/server/health"
)

// configHost implements only GetConfig; any other Host call panics on the
// nil embedded interface, proving the action touches nothing else.
type configHost struct {
	pluginsdk.Host
	config map[string]any
}

func (h configHost) GetConfig(context.Context) (map[string]any, error) { return h.config, nil }

type okAPI struct{}

func (okAPI) CurrentUser(context.Context) (discord.User, error) {
	return discord.User{Username: "bot"}, nil
}
func (okAPI) CurrentApplication(context.Context) (discord.Application, error) {
	return discord.Application{ID: "123456789012345678", Flags: discord.FlagGatewayMessageContentLimited}, nil
}
func (okAPI) CurrentUserGuilds(context.Context) ([]discord.PartialGuild, error) {
	return []discord.PartialGuild{{ID: "223456789012345678", Name: "Team", Permissions: strconv.FormatUint(discord.RequiredPermissionBits(), 10)}}, nil
}

func newTestPlugin(cfg map[string]any) *discordPlugin {
	p := &discordPlugin{newAPI: func(string) health.API { return okAPI{} }}
	if cfg != nil {
		p.SetHost(configHost{config: cfg})
	}
	return p
}

func TestConnectionTestActionUsesVerifiedWorkspace(t *testing.T) {
	p := newTestPlugin(map[string]any{
		"bot_token":        "tok",
		"application_id":   "123456789012345678",
		"workspace_guilds": "ws-1=223456789012345678",
	})
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionConnectionTest,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 200, resp.Status)
	var result health.Result
	require.NoError(t, json.Unmarshal(resp.Body, &result))
	assert.True(t, result.OK)
	assert.Equal(t, "ws-1", result.WorkspaceID)
}

func TestUnknownActionIs404(t *testing.T) {
	resp, err := newTestPlugin(map[string]any{}).HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: "nope"})
	require.NoError(t, err)
	assert.Equal(t, 404, resp.Status)
}

func TestActionBeforeHostInjectionIs503(t *testing.T) {
	resp, err := newTestPlugin(nil).HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionConnectionTest})
	require.NoError(t, err)
	assert.Equal(t, 503, resp.Status)
}
