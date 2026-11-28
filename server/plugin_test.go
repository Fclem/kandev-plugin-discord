package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/health"
	"kandev-plugin-discord/server/store"
)

// configHost implements GetConfig and Host state; any other Host call panics
// on the nil embedded interface, proving the actions touch nothing else.
type configHost struct {
	pluginsdk.Host
	*store.MemoryHost
	config map[string]any
}

func (h configHost) GetConfig(context.Context) (map[string]any, error) { return h.config, nil }

func (h configHost) GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	return h.MemoryHost.GetState(ctx, scope, scopeID, key)
}

func (h configHost) SetState(ctx context.Context, scope, scopeID, key string, value map[string]any) error {
	return h.MemoryHost.SetState(ctx, scope, scopeID, key, value)
}

func (h configHost) DeleteState(ctx context.Context, scope, scopeID, key string) error {
	return h.MemoryHost.DeleteState(ctx, scope, scopeID, key)
}

func (h configHost) ListState(ctx context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	return h.MemoryHost.ListState(ctx, scope, scopeID)
}

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
		p.SetHost(configHost{MemoryHost: store.NewMemoryHost(), config: cfg})
	}
	return p
}

func TestSetHostStartsTheBridgeOnce(t *testing.T) {
	started := make(chan pluginsdk.Host, 2)
	p := &discordPlugin{startBridge: func(h pluginsdk.Host) { started <- h }}
	h := configHost{MemoryHost: store.NewMemoryHost()}
	p.SetHost(h)
	p.SetHost(h)
	<-started
	assert.Empty(t, started)
	assert.NotNil(t, p.Host())
}

func TestConnectionStatusServesCachedHealthWithoutDiscord(t *testing.T) {
	h := configHost{MemoryHost: store.NewMemoryHost(), config: map[string]any{"bot_token": "MTIz.secret"}}
	require.NoError(t, store.New(h).PutHealth(context.Background(), store.Health{
		GatewayState: "fatal", LastCloseCode: 4014, Reason: "disallowed intents (close code 4014)",
		Bindings: []store.BindingHealth{{WorkspaceID: "ws-1", GuildID: "223456789012345678", BotMember: true, Provisioned: true}},
	}))
	p := &discordPlugin{newAPI: func(string) health.API { panic("connection.status must not call Discord") }}
	p.SetHost(h)
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionConnectionStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 200, resp.Status)
	assert.NotContains(t, string(resp.Body), "MTIz")
	var status health.GatewayStatus
	require.NoError(t, json.Unmarshal(resp.Body, &status))
	assert.Equal(t, "fatal", status.State)
	assert.Equal(t, 4014, status.CloseCode)
	assert.True(t, status.Provisioned)
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

// failingConfigHost fails GetConfig a number of times before answering.
type failingConfigHost struct {
	configHost
	failures *int
}

func (h failingConfigHost) GetConfig(ctx context.Context) (map[string]any, error) {
	if *h.failures > 0 {
		*h.failures--
		return nil, errors.New("vault unavailable")
	}
	return h.configHost.GetConfig(ctx)
}

func TestUnreadableSettingsAreRetriedAndReplaceStaleHealth(t *testing.T) {
	ctx := context.Background()
	failures := 2
	h := failingConfigHost{configHost{MemoryHost: store.NewMemoryHost(), config: map[string]any{"bot_token": "MTIz.secret"}}, &failures}
	st := store.New(h)
	require.NoError(t, st.PutHealth(ctx, store.Health{GatewayState: "connected"})) // the previous process's
	var waits []time.Duration
	raw := readSettings(ctx, h, st, func(d time.Duration) <-chan time.Time {
		got, ok, err := st.Health(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "error", got.GatewayState, "the card no longer shows the stale state while retrying")
		waits = append(waits, d)
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	})
	assert.Equal(t, "MTIz.secret", raw["bot_token"])
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second}, waits)
}
