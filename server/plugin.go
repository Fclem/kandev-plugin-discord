package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/bridge"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/health"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

// Manifest actions the settings card invokes. Kandev authenticates the
// caller, enforces `access: admin`, and verifies the workspace before the
// request reaches the plugin.
const (
	actionConnectionTest   = "connection.test"
	actionConnectionStatus = "connection.status"
)

// discordPlugin is the plugin backend: the settings card's admin actions and
// the Discord bridge, started once when Kandev injects the Host.
type discordPlugin struct {
	pluginsdk.UnimplementedPlugin

	// newAPI builds the Discord client for a token; replaced in tests.
	newAPI func(token string) health.API
	// startBridge runs the bridge; replaced in tests.
	startBridge func(h pluginsdk.Host)

	once   sync.Once
	mu     sync.Mutex
	bridge *bridge.Bridge
}

var (
	_ pluginsdk.Plugin        = (*discordPlugin)(nil)
	_ pluginsdk.ActionHandler = (*discordPlugin)(nil)
	_ pluginsdk.HostSetter    = (*discordPlugin)(nil)
)

func newDiscordPlugin() *discordPlugin {
	p := &discordPlugin{newAPI: func(token string) health.API { return discordapi.NewClient(discordapi.NewSession(token)) }}
	p.startBridge = p.runBridge
	return p
}

// SetHost stores the Host and starts the bridge once.
func (p *discordPlugin) SetHost(h pluginsdk.Host) {
	p.UnimplementedPlugin.SetHost(h)
	if h == nil || p.startBridge == nil {
		return
	}
	p.once.Do(func() { go p.startBridge(h) })
}

// runBridge reads the settings once (saving them restarts the plugin) and
// runs the Discord bridge until the gateway stops for good.
func (p *discordPlugin) runBridge(h pluginsdk.Host) {
	ctx := context.Background()
	st := store.New(h)
	raw, err := h.GetConfig(ctx)
	if err != nil {
		log.Printf("discord: reading settings: %v", err)
		return
	}
	cfg, cfgErr := config.Parse(raw)
	if cfgErr != nil {
		log.Printf("discord: settings are incomplete; the bridge stays off: %v", cfgErr)
		if err := st.PutHealth(ctx, store.Health{GatewayState: "not_configured", ConfigError: cfgErr.Error()}); err != nil {
			log.Printf("discord: saving health: %v", err)
		}
		return
	}
	sess := discordapi.NewSession(cfg.BotToken)
	api := discordapi.NewClient(sess)
	b := bridge.New(bridge.Deps{
		Context: ctx, Config: cfg, API: api, Host: kandev.SDK{H: h}, Store: st,
		Halted: api.Halted, HaltNotifier: api,
	})
	p.mu.Lock()
	p.bridge = b
	p.mu.Unlock()
	final := b.Run(sess)
	log.Printf("discord: gateway stopped: %s", final.Name)
}

func (p *discordPlugin) currentBridge() *bridge.Bridge {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bridge
}

// OnEvent forwards Kandev events to the running bridge. Events are only
// triggers, so dropping them before the bridge starts loses nothing: the
// first reconcile after READY catches up.
func (p *discordPlugin) OnEvent(ctx context.Context, e *pluginsdk.Event) error {
	if b := p.currentBridge(); b != nil {
		b.HandleEvent(ctx, e)
	}
	return nil
}

// HandleAction serves host-authenticated browser actions.
func (p *discordPlugin) HandleAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req.ActionKey != actionConnectionTest && req.ActionKey != actionConnectionStatus {
		return jsonResponse(404, map[string]string{"error": "unknown action"}), nil
	}
	host := p.Host()
	if host == nil {
		return jsonResponse(503, map[string]string{"error": "plugin is still starting; retry"}), nil
	}
	st := store.New(host)
	cached, _, err := st.Health(ctx)
	if err != nil {
		log.Printf("%s: reading health: %v", req.ActionKey, err)
	}
	if req.ActionKey == actionConnectionStatus {
		return jsonResponse(200, health.Status(cached, req.Context.WorkspaceID)), nil
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
	result.Gateway = health.Status(cached, req.Context.WorkspaceID)
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
