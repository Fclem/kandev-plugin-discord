package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

const (
	appID   = "123456789012345678"
	guildID = "223456789012345678"
	wsID    = "ws-1"
	botID   = "900000000000000001"
)

type harness struct {
	b     *Bridge
	api   *discordapi.Fake
	host  *kandev.Fake
	mem   *store.MemoryHost
	st    *store.Store
	clock *time.Time
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg, err := config.Parse(map[string]any{
		"bot_token": "MTIz.secret", "application_id": appID, "workspace_guilds": wsID + "=" + guildID,
		"kandev_base_url": "https://kandev.example.com",
	})
	require.NoError(t, err)
	for _, m := range mutate {
		m(&cfg)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	h := &harness{api: discordapi.NewFake(), host: kandev.NewFake(), mem: store.NewMemoryHost(), clock: &now}
	h.st = store.New(h.mem)
	h.b = New(Deps{Config: cfg, API: h.api, Host: h.host, Store: h.st, Now: func() time.Time { return *h.clock }})
	h.b.bind(context.Background())
	return h
}

// ready simulates READY + GUILD_CREATE for the bound guild and waits for
// provisioning (and whatever it schedules).
func (h *harness) ready(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	h.b.onReady(ctx, &discordgo.Ready{User: &discordgo.User{ID: botID}, Guilds: []*discordgo.Guild{{ID: guildID}}})
	h.b.onGuildCreate(ctx, &discordgo.Guild{ID: guildID})
	h.wait()
	_, ok := h.b.guild(wsID)
	require.True(t, ok, "guild provisioned")
}

func (h *harness) wait() { h.b.sched.Wait() }

func TestReadyReportsMissingGuildWithInvite(t *testing.T) {
	h := newHarness(t)
	h.b.onReady(context.Background(), &discordgo.Ready{User: &discordgo.User{ID: botID}})
	rec, ok, err := h.st.Health(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, rec.Bindings, 1)
	assert.False(t, rec.Bindings[0].BotMember)
	assert.Contains(t, rec.Bindings[0].InviteURL, "guild_id="+guildID)
}

func TestGuildCreateProvisionsBoundGuildOnly(t *testing.T) {
	h := newHarness(t)
	h.b.onGuildCreate(context.Background(), &discordgo.Guild{ID: "999999999999999999"})
	h.wait()
	assert.Empty(t, h.api.Calls(), "unbound guild ignored")

	h.ready(t)
	rec, _, err := h.st.Health(context.Background())
	require.NoError(t, err)
	assert.True(t, rec.Bindings[0].Provisioned)
	g, ok, err := h.st.Guild(context.Background(), wsID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, guildID, g.GuildID)
}
