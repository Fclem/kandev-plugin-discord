package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/bridge"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/health"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

func TestIncompleteSettingsLeaveTheBridgeOffWithAReason(t *testing.T) {
	h := configHost{MemoryHost: store.NewMemoryHost(), config: map[string]any{"bot_token": "MTIz.secret"}}
	newDiscordPlugin().runBridge(h) // returns instead of connecting

	got, ok, err := store.New(h).Health(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "not_configured", got.GatewayState)
	assert.NotEmpty(t, got.ConfigError)
	assert.NotContains(t, got.ConfigError, "MTIz", "the token never reaches health")
}

func TestConnectionTestReportsMissingToken(t *testing.T) {
	p := &discordPlugin{newAPI: func(string) health.API { panic("no token, no Discord client") }}
	p.SetHost(configHost{MemoryHost: store.NewMemoryHost(), config: map[string]any{}})
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionConnectionTest, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 200, resp.Status)
	assert.Equal(t, "application/json", resp.Headers["Content-Type"])
	var result health.Result
	require.NoError(t, json.Unmarshal(resp.Body, &result))
	assert.False(t, result.OK)
	assert.Equal(t, "ws-1", result.WorkspaceID)
	assert.Equal(t, "unknown", result.Gateway.State, "no cached health yet")
}

// brokenConfigHost fails GetConfig and Host state reads.
type brokenConfigHost struct{ configHost }

func (brokenConfigHost) GetConfig(context.Context) (map[string]any, error) {
	return nil, errors.New("vault unavailable")
}

func (brokenConfigHost) GetState(context.Context, string, string, string) (map[string]any, bool, error) {
	return nil, false, errors.New("state unavailable")
}

func TestConnectionTestWithUnreadableSettingsIs503(t *testing.T) {
	p := &discordPlugin{newAPI: func(string) health.API { panic("unreachable") }}
	p.SetHost(brokenConfigHost{configHost{MemoryHost: store.NewMemoryHost()}})
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionConnectionTest})
	require.NoError(t, err)
	assert.Equal(t, 503, resp.Status)
	assert.JSONEq(t, `{"error":"could not read plugin settings"}`, string(resp.Body))
	assert.NotContains(t, string(resp.Body), "vault", "host errors stay in the log")

	status, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionConnectionStatus})
	require.NoError(t, err)
	assert.Equal(t, 200, status.Status, "unreadable health still answers")
}

func TestJSONResponseEncodingFailureIs500(t *testing.T) {
	resp := jsonResponse(200, map[string]any{"bad": make(chan int)})
	assert.Equal(t, 500, resp.Status)
	assert.JSONEq(t, `{"error":"encoding response"}`, string(resp.Body))
}

func TestEventsBeforeTheBridgeAreDropped(t *testing.T) {
	p := newDiscordPlugin()
	assert.NoError(t, p.OnEvent(context.Background(), &pluginsdk.Event{EventID: "e1", EventType: "task.updated"}))
	assert.Nil(t, p.currentBridge())
}

// lookupHost reports each task the bridge reads.
type lookupHost struct {
	*kandev.Fake
	reads chan string
}

func (h lookupHost) GetTask(ctx context.Context, id string) (*pluginsdk.Task, error) {
	h.reads <- id
	return h.Fake.GetTask(ctx, id)
}

func TestEventsReachTheRunningBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := lookupHost{Fake: kandev.NewFake(), reads: make(chan string, 1)}
	p := newDiscordPlugin()
	p.bridge = bridge.New(bridge.Deps{
		Context: ctx, Config: config.Config{}, API: discordapi.NewFake(), Host: host, Store: store.New(store.NewMemoryHost()),
	})
	require.NoError(t, p.OnEvent(ctx, &pluginsdk.Event{EventID: "e1", EventType: "task.updated", Payload: map[string]any{"task_id": "t1"}}))
	select {
	case id := <-host.reads:
		assert.Equal(t, "t1", id, "the event triggers a sync of its task")
	case <-time.After(10 * time.Second):
		t.Fatal("the event never reached the bridge")
	}
}
