package discord

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientSendsBotAuthAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bot tok", r.Header.Get("Authorization"))
		assert.True(t, strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot ("))
		switch r.URL.Path {
		case "/users/@me":
			_, _ = w.Write([]byte(`{"id":"1","username":"kandev","bot":true}`))
		case "/users/@me/guilds":
			_, _ = w.Write([]byte(`[{"id":"9","name":"Team","permissions":"2048"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}

	u, err := c.CurrentUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, User{ID: "1", Username: "kandev", Bot: true}, u)

	gs, err := c.CurrentUserGuilds(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []PartialGuild{{ID: "9", Name: "Team", Permissions: "2048"}}, gs)
}

func TestClientMapsErrorsWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You are being rate limited.","retry_after":1.5,"code":0}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "super-secret", HTTP: srv.Client()}

	_, err := c.CurrentApplication(context.Background())
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusTooManyRequests, apiErr.Status)
	assert.Contains(t, err.Error(), "retry after 1.5s")
	assert.NotContains(t, err.Error(), "super-secret")
}

func TestMissingPermissions(t *testing.T) {
	assert.Empty(t, MissingPermissions(RequiredPermissionBits()))
	assert.Empty(t, MissingPermissions(Administrator), "administrator grants everything")
	missing := MissingPermissions(RequiredPermissionBits() &^ (1 << 28) &^ (1 << 51))
	assert.Equal(t, []string{"MANAGE_ROLES", "PIN_MESSAGES"}, missing)
}

func TestInviteURL(t *testing.T) {
	parsed, err := url.Parse(InviteURL("123", "456"))
	require.NoError(t, err)
	q := parsed.Query()
	assert.Equal(t, "123", q.Get("client_id"))
	assert.Equal(t, "456", q.Get("guild_id"))
	assert.Equal(t, "bot applications.commands", q.Get("scope"))
	perms, err := strconv.ParseUint(q.Get("permissions"), 10, 64)
	require.NoError(t, err)
	assert.Equal(t, RequiredPermissionBits(), perms)
	assert.Zero(t, perms&Administrator, "never request ADMINISTRATOR")
}

func TestMessageContentIntentEnabled(t *testing.T) {
	assert.False(t, MessageContentIntentEnabled(0))
	assert.True(t, MessageContentIntentEnabled(FlagGatewayMessageContentLimited))
	assert.True(t, MessageContentIntentEnabled(FlagGatewayMessageContent))
}
