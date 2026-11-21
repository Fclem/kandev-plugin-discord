package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discord"
	"kandev-plugin-discord/server/health"
)

// actionConnectionTest is the manifest action the settings card invokes.
// Kandev authenticates the caller, enforces `access: admin`, and verifies the
// workspace before the request reaches the plugin.
const actionConnectionTest = "connection.test"

// discordPlugin is the plugin backend. Today it serves the settings card's
// connection test; the gateway bridge lands per docs/design/plan.md.
type discordPlugin struct {
	pluginsdk.UnimplementedPlugin

	// newAPI builds the Discord client for a token; replaced in tests.
	newAPI func(token string) health.API
}

var (
	_ pluginsdk.Plugin        = (*discordPlugin)(nil)
	_ pluginsdk.ActionHandler = (*discordPlugin)(nil)
)

func newDiscordPlugin() *discordPlugin {
	return &discordPlugin{newAPI: func(token string) health.API { return discord.NewClient(token) }}
}

// HandleAction serves host-authenticated browser actions.
func (p *discordPlugin) HandleAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req.ActionKey != actionConnectionTest {
		return jsonResponse(404, map[string]string{"error": "unknown action"}), nil
	}
	host := p.Host()
	if host == nil {
		return jsonResponse(503, map[string]string{"error": "plugin is still starting; retry"}), nil
	}
	raw, err := host.GetConfig(ctx)
	if err != nil {
		log.Printf("connection.test: reading config: %v", err)
		return jsonResponse(503, map[string]string{"error": "could not read plugin settings"}), nil
	}
	cfg, cfgErr := config.Parse(raw)
	var api health.API
	if cfg.BotToken != "" {
		api = p.newAPI(cfg.BotToken)
	}
	result := health.Test(ctx, api, cfg, cfgErr, req.Context.WorkspaceID)
	log.Printf("connection.test: workspace=%s ok=%t", req.Context.WorkspaceID, result.OK)
	return jsonResponse(200, result), nil
}

func jsonResponse(status int, body any) *pluginsdk.PluginActionResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		return &pluginsdk.PluginActionResponse{Status: 500, Body: []byte(`{"error":"encoding response"}`)}
	}
	return &pluginsdk.PluginActionResponse{
		Status:  status,
		Body:    raw,
		Headers: map[string]string{"Content-Type": "application/json"},
	}
}
