package discordapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rewrite sends every request to the test server, keeping the path.
type rewrite struct{ target *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	s := NewSession("secret-token")
	s.Client = &http.Client{Transport: rewrite{target}}
	s.MaxRestRetries = 0
	return NewClient(s)
}

func TestClientSendsBotAuthAndReadsApplicationFlags(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bot secret-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v" + discordgo.APIVersion + "/applications/@me":
			_, _ = w.Write([]byte(`{"id":"123456789012345678","name":"Kandev","flags":524288}`))
		case "/api/v" + discordgo.APIVersion + "/users/@me":
			_, _ = w.Write([]byte(`{"id":"1","username":"kandev","bot":true}`))
		default:
			http.NotFound(w, r)
		}
	})
	app, err := c.CurrentApplication(context.Background())
	require.NoError(t, err)
	assert.True(t, MessageContentIntentEnabled(uint64(app.Flags)))
	u, err := c.CurrentUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "kandev", u.Username)
}

func TestClientHaltsAfterBotTokenRejected(t *testing.T) {
	var hits atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":0,"message":"401: Unauthorized"}`))
	})
	_, err := c.CurrentUser(context.Background())
	require.ErrorIs(t, err, ErrHalted)
	assert.Equal(t, http.StatusUnauthorized, Status(err))
	assert.NotContains(t, err.Error(), "secret-token")
	assert.True(t, c.Halted())

	_, err = c.GuildRoles(context.Background(), "1")
	require.ErrorIs(t, err, ErrHalted)
	assert.Equal(t, int32(1), hits.Load(), "no request after the halt")
}

func TestClientInteractionUnauthorizedDoesNotHalt(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":50027,"message":"Invalid Webhook Token"}`))
	})
	err := c.InteractionRespond(context.Background(), &discordgo.Interaction{ID: "1", Token: "t", AppID: "2"},
		&discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrHalted))
	assert.False(t, c.Halted())
}

func TestClientMapsDiscordErrorCodes(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":30013,"message":"Maximum number of guild channels reached (500)"}`))
	})
	_, err := c.GuildChannelCreate(context.Background(), "1", discordgo.GuildChannelCreateData{Name: "x"})
	require.Error(t, err)
	assert.True(t, HasCode(err, CodeMaxGuildChannels))
	assert.Equal(t, http.StatusBadRequest, Status(err))
}

func TestClientMessagesNeverParseMentions(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		_, _ = w.Write([]byte(`{"id":"5","channel_id":"7"}`))
	})
	_, err := c.MessageSend(context.Background(), "7", &discordgo.MessageSend{
		Content:         "@everyone hi",
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}},
	})
	require.NoError(t, err)
	mentions, ok := body["allowed_mentions"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{}, mentions["parse"])
	assert.True(t, strings.Contains(body["content"].(string), "@everyone"))
}

func TestFakeChannelMessagesAfterCursor(t *testing.T) {
	f := NewFake()
	ch := f.AddChannel(&discordgo.Channel{GuildID: "g"})
	a := f.AddMessage(&discordgo.Message{ChannelID: ch.ID})
	b := f.AddMessage(&discordgo.Message{ChannelID: ch.ID})
	c := f.AddMessage(&discordgo.Message{ChannelID: ch.ID})
	got, err := f.ChannelMessages(context.Background(), ch.ID, 100, a.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{c.ID, b.ID}, []string{got[0].ID, got[1].ID}, "newest first like Discord")
	assert.Equal(t, 0, f.Mutations())
	assert.Less(t, CompareIDs("999", "1000"), 0)
}
