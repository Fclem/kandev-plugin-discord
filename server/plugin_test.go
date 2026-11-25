package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/health"
)

// configHost implements only GetConfig; any other Host call panics on the
// nil embedded interface, proving the action touches nothing else.
type configHost struct {
	pluginsdk.Host
	config map[string]any
}

func (h configHost) GetConfig(context.Context) (map[string]any, error) { return h.config, nil }

func healthyFake() *discordapi.Fake {
	f := discordapi.NewFake()
	f.Application = &discordgo.Application{ID: "123456789012345678", Flags: int(discordapi.FlagGatewayMessageContentLimited)}
	f.AddGuild("223456789012345678", "Team", int64(discordapi.RequiredPermissionBits()))
	return f
}

func newTestPlugin(cfg map[string]any) *discordPlugin {
	f := healthyFake()
	p := &discordPlugin{newAPI: func(string) health.API { return f }}
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
