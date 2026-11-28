package discordapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var apiPrefix = "/api/v" + discordgo.APIVersion

// request is one request the test Discord server received.
type request struct {
	Method, Path, Query string
	Body                map[string]any
	RawBody             string
}

// recorder is a test Discord server answering each "METHOD path" with a
// canned status and body, and recording every request.
type recorder struct {
	mu        sync.Mutex
	requests  []request
	responses map[string]response
}

type response struct {
	status int
	body   string
}

func newRecorder(responses map[string]response) *recorder {
	return &recorder{responses: responses}
}

func (r *recorder) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		rec := request{Method: req.Method, Path: strings.TrimPrefix(req.URL.Path, apiPrefix), Query: req.URL.RawQuery, RawBody: string(raw)}
		if len(raw) > 0 && raw[0] == '{' {
			assert.NoError(t, json.Unmarshal(raw, &rec.Body))
		}
		r.mu.Lock()
		r.requests = append(r.requests, rec)
		resp, ok := r.responses[req.Method+" "+rec.Path]
		r.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":0,"message":"404: Not Found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if resp.status == 0 {
			resp.status = http.StatusOK
		}
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}
}

func (r *recorder) all() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.requests...)
}

func (r *recorder) last(t *testing.T) request {
	t.Helper()
	all := r.all()
	require.NotEmpty(t, all, "no request reached the server")
	return all[len(all)-1]
}

func TestClientReadCallsHitDiscordRoutes(t *testing.T) {
	rec := newRecorder(map[string]response{
		"GET /users/@me/guilds":                  {body: `[{"id":"10","name":"Guild","permissions":"8"}]`},
		"GET /guilds/10/members/20":              {body: `{"user":{"id":"20"},"roles":["30","31"]}`},
		"GET /guilds/10/roles":                   {body: `[{"id":"30","name":"kandev-admin"}]`},
		"GET /guilds/10/channels":                {body: `[{"id":"40","name":"kandev","type":4},{"id":"41","name":"task","type":0,"parent_id":"40"}]`},
		"GET /channels/41/messages":              {body: `[{"id":"52","channel_id":"41"},{"id":"51","channel_id":"41"}]`},
		"GET /applications/@me":                  {body: `{"id":"7","flags":262144}`},
		"GET /users/@me":                         {body: `{"id":"1","username":"kandev","bot":true}`},
		"PUT /applications/7/guilds/10/commands": {body: `[]`},
	})
	c := testClient(t, rec.handler(t))
	ctx := context.Background()

	guilds, err := c.CurrentUserGuilds(ctx)
	require.NoError(t, err)
	require.Len(t, guilds, 1)
	assert.Equal(t, int64(Administrator), guilds[0].Permissions)
	assert.Contains(t, rec.last(t).Query, "limit=200")

	m, err := c.GuildMember(ctx, "10", "20")
	require.NoError(t, err)
	assert.Equal(t, []string{"30", "31"}, m.Roles)

	roles, err := c.GuildRoles(ctx, "10")
	require.NoError(t, err)
	assert.Equal(t, "kandev-admin", roles[0].Name)

	chans, err := c.GuildChannels(ctx, "10")
	require.NoError(t, err)
	require.Len(t, chans, 2)
	assert.Equal(t, "40", chans[1].ParentID)

	msgs, err := c.ChannelMessages(ctx, "41", 50, "50")
	require.NoError(t, err)
	assert.Equal(t, []string{"52", "51"}, []string{msgs[0].ID, msgs[1].ID})
	q := rec.last(t).Query
	assert.Contains(t, q, "limit=50")
	assert.Contains(t, q, "after=50")
	assert.NotContains(t, q, "before=")

	app, err := c.CurrentApplication(ctx)
	require.NoError(t, err)
	assert.True(t, MessageContentIntentEnabled(uint64(app.Flags)))

	require.NoError(t, c.BulkOverwriteGuildCommands(ctx, "7", "10", []*discordgo.ApplicationCommand{{Name: "kandev", Description: "Kandev"}}))
	put := rec.last(t)
	assert.Equal(t, "PUT", put.Method)
	assert.Contains(t, put.RawBody, `"name":"kandev"`)
}

func TestClientMutationsSendExpectedRequests(t *testing.T) {
	rec := newRecorder(map[string]response{
		"PATCH /channels/41":                           {body: `{"id":"41","name":"renamed","parent_id":"40"}`},
		"DELETE /channels/41":                          {body: `{"id":"41"}`},
		"PATCH /channels/41/messages/51":               {body: `{"id":"51","channel_id":"41","content":"edited"}`},
		"PUT /channels/41/messages/51/reactions/✅/@me": {status: http.StatusNoContent},
		"PUT /channels/41/pins/51":                     {status: http.StatusNoContent},
		"POST /guilds/10/roles":                        {body: `{"id":"30","name":"kandev-admin"}`},
		"POST /guilds/10/channels":                     {body: `{"id":"42","name":"new","type":0}`},
	})
	c := testClient(t, rec.handler(t))
	ctx := context.Background()

	ch, err := c.ChannelEdit(ctx, "41", &discordgo.ChannelEdit{Name: "renamed", ParentID: "40"})
	require.NoError(t, err)
	assert.Equal(t, "renamed", ch.Name)
	edit := rec.last(t)
	assert.Equal(t, "PATCH", edit.Method)
	assert.Equal(t, "renamed", edit.Body["name"])
	assert.Equal(t, "40", edit.Body["parent_id"])

	require.NoError(t, c.ChannelDelete(ctx, "41"))
	assert.Equal(t, request{Method: "DELETE", Path: "/channels/41"}, rec.last(t))

	content := "edited @everyone"
	msg, err := c.MessageEdit(ctx, &discordgo.MessageEdit{ID: "51", Channel: "41", Content: &content,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}}})
	require.NoError(t, err)
	assert.Equal(t, "edited", msg.Content)
	medit := rec.last(t)
	assert.Equal(t, content, medit.Body["content"])
	assert.Equal(t, []any{}, medit.Body["allowed_mentions"].(map[string]any)["parse"], "edits never re-enable mentions")

	require.NoError(t, c.ReactionAdd(ctx, "41", "51", "✅"))
	assert.Equal(t, "PUT", rec.last(t).Method)
	require.NoError(t, c.MessagePin(ctx, "41", "51"))
	assert.Equal(t, "/channels/41/pins/51", rec.last(t).Path)

	role, err := c.GuildRoleCreate(ctx, "10", &discordgo.RoleParams{Name: "kandev-admin"})
	require.NoError(t, err)
	assert.Equal(t, "30", role.ID)
	assert.Equal(t, "kandev-admin", rec.last(t).Body["name"])

	created, err := c.GuildChannelCreate(ctx, "10", discordgo.GuildChannelCreateData{Name: "new", Topic: "t", ParentID: "40"})
	require.NoError(t, err)
	assert.Equal(t, "42", created.ID)
	body := rec.last(t).Body
	assert.Equal(t, "new", body["name"])
	assert.Equal(t, "40", body["parent_id"])
}

func TestMessageSendOnceReturnsMessageAndDefaultsEmbedType(t *testing.T) {
	rec := newRecorder(map[string]response{
		"POST /channels/7/messages": {body: `{"id":"5","channel_id":"7","content":"hello"}`},
	})
	c := testClient(t, rec.handler(t))
	embed := &discordgo.MessageEmbed{Title: "Task"}
	typed := &discordgo.MessageEmbed{Title: "Image", Type: discordgo.EmbedTypeImage}
	m, err := c.MessageSendOnce(context.Background(), "7", &discordgo.MessageSend{Content: "hello", Embeds: []*discordgo.MessageEmbed{embed, typed}}, "nonce-1")
	require.NoError(t, err)
	assert.Equal(t, "5", m.ID)

	body := rec.last(t).Body
	assert.Equal(t, "nonce-1", body["nonce"])
	assert.Equal(t, true, body["enforce_nonce"])
	embeds := body["embeds"].([]any)
	assert.Equal(t, "rich", embeds[0].(map[string]any)["type"])
	assert.Equal(t, "image", embeds[1].(map[string]any)["type"], "an explicit embed type is kept")
}

func TestMessageSendOnceRejectsMalformedReply(t *testing.T) {
	rec := newRecorder(map[string]response{"POST /channels/7/messages": {body: `not json`}})
	c := testClient(t, rec.handler(t))
	_, err := c.MessageSendOnce(context.Background(), "7", &discordgo.MessageSend{Content: "x"}, "n")
	require.Error(t, err)
	assert.Zero(t, Status(err), "a decode failure is not a Discord HTTP error")
}

func TestCurrentApplicationErrors(t *testing.T) {
	rec := newRecorder(map[string]response{"GET /applications/@me": {body: `{"id":`}})
	c := testClient(t, rec.handler(t))
	_, err := c.CurrentApplication(context.Background())
	require.Error(t, err, "malformed reply")
	assert.False(t, c.Halted())

	rec.responses["GET /applications/@me"] = response{status: http.StatusForbidden, body: `{"code":50001,"message":"Missing Access"}`}
	_, err = c.CurrentApplication(context.Background())
	assert.True(t, IsForbidden(err))
}

func TestInteractionEditNeverParsesMentions(t *testing.T) {
	rec := newRecorder(map[string]response{
		"PATCH /webhooks/2/tok/messages/@original": {body: `{"id":"9"}`},
	})
	c := testClient(t, rec.handler(t))
	content := "@everyone done"
	err := c.InteractionEdit(context.Background(), &discordgo.Interaction{ID: "1", Token: "tok", AppID: "2"},
		&discordgo.WebhookEdit{Content: &content})
	require.NoError(t, err)
	body := rec.last(t).Body
	assert.Equal(t, content, body["content"])
	assert.Equal(t, []any{}, body["allowed_mentions"].(map[string]any)["parse"])
}

func TestInteractionRespondNeverParsesMentions(t *testing.T) {
	rec := newRecorder(map[string]response{
		"POST /interactions/1/tok/callback": {status: http.StatusNoContent},
	})
	c := testClient(t, rec.handler(t))
	err := c.InteractionRespond(context.Background(), &discordgo.Interaction{ID: "1", Token: "tok", AppID: "2"},
		&discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "@here ok", AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone},
			}},
		})
	require.NoError(t, err)
	data := rec.last(t).Body["data"].(map[string]any)
	assert.Equal(t, []any{}, data["allowed_mentions"].(map[string]any)["parse"])
}

func TestClientErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		gone      bool
		forbidden bool
		code      int
	}{
		{"unknown channel", http.StatusNotFound, `{"code":10003,"message":"Unknown Channel"}`, true, false, CodeUnknownChannel},
		{"unknown message", http.StatusNotFound, `{"code":10008,"message":"Unknown Message"}`, true, false, CodeUnknownMessage},
		{"bare 404", http.StatusNotFound, ``, true, false, 0},
		{"missing access", http.StatusForbidden, `{"code":50001,"message":"Missing Access"}`, false, true, CodeMissingAccess},
		{"missing permissions", http.StatusForbidden, `{"code":50013,"message":"Missing Permissions"}`, false, true, CodeMissingPermissions},
		{"pins full", http.StatusBadRequest, `{"code":30003,"message":"Maximum number of pins reached (50)"}`, false, false, CodeMaxPinsReached},
		{"server error", http.StatusInternalServerError, ``, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := c.MessagePin(context.Background(), "41", "51")
			require.Error(t, err)
			assert.Equal(t, tc.status, Status(err))
			assert.Equal(t, tc.gone, IsGone(err))
			assert.Equal(t, tc.forbidden, IsForbidden(err))
			if tc.code != 0 {
				assert.True(t, HasCode(err, tc.code))
			}
			assert.False(t, errors.Is(err, ErrHalted), "only 401 halts")
			assert.False(t, c.Halted())
			assert.NotContains(t, err.Error(), "secret-token")
		})
	}
}

func TestHaltDuringBudgetWaitStopsTheQueuedCall(t *testing.T) {
	rec := newRecorder(map[string]response{"GET /users/@me": {body: `{"id":"1"}`}})
	c := testClient(t, rec.handler(t))
	reserved := time.Now().Add(250 * time.Millisecond)
	c.limiter.mu.Lock()
	c.limiter.next = reserved
	c.limiter.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := c.CurrentUser(context.Background())
		done <- err
	}()
	// Wait until the call has taken its slot and is waiting for it.
	require.Eventually(t, func() bool {
		c.limiter.mu.Lock()
		defer c.limiter.mu.Unlock()
		return c.limiter.next.After(reserved)
	}, 5*time.Second, time.Millisecond)
	c.halted.Store(true) // a concurrent call's 401 lands while this one waits
	require.ErrorIs(t, <-done, ErrHalted)
	assert.Empty(t, rec.all(), "no request after the halt")
}

func TestIsGoneRecognizesCodesWithoutNotFoundStatus(t *testing.T) {
	assert.True(t, IsGone(&Error{Status: http.StatusBadRequest, Code: CodeUnknownMessage}))
	assert.True(t, IsGone(&Error{Status: http.StatusBadRequest, Code: CodeUnknownChannel}))
	assert.False(t, IsGone(&Error{Status: http.StatusBadRequest, Code: CodeMaxGuildChannels}))
	assert.False(t, IsGone(errors.New("network")))
	assert.False(t, IsForbidden(nil))
	assert.Zero(t, Status(errors.New("plain")))
	assert.False(t, HasCode(nil, CodeUnknownChannel))
}

func TestErrorMessageFormats(t *testing.T) {
	assert.Equal(t, "discord API returned HTTP 500", (&Error{Status: 500}).Error())
	assert.Equal(t, "discord API returned HTTP 403 (code 50013)", (&Error{Status: 403, Code: 50013}).Error())
	assert.Equal(t, "discord API returned HTTP 400 (code 30013): too many", (&Error{Status: 400, Code: 30013, Message: "too many"}).Error())
	assert.Equal(t, "discord API returned HTTP 502: lost", (&Error{Status: 502, Message: "lost"}).Error())
	wrapped := errors.Join(errors.New("context"), &Error{Status: 404, Code: CodeUnknownChannel})
	assert.True(t, IsGone(wrapped), "classification sees through wrapping")
}

func TestCancelledContextStopsBudgetWait(t *testing.T) {
	rec := newRecorder(map[string]response{"GET /users/@me": {body: `{"id":"1"}`}})
	c := testClient(t, rec.handler(t))
	c.limiter.mu.Lock()
	c.limiter.next = time.Now().Add(time.Hour)
	c.limiter.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CurrentUser(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, rec.all())
}

func TestLimiterSpacesCallsAtTheConfiguredRate(t *testing.T) {
	l := newLimiter(1000) // 1 ms interval
	start := time.Now()
	for range 5 {
		require.NoError(t, l.wait(context.Background()))
	}
	assert.GreaterOrEqual(t, time.Since(start), 4*time.Millisecond, "five calls take at least four intervals")

	l = newLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, l.wait(ctx), context.Canceled, "an immediate slot still honours a cancelled context")
}

func TestTransportErrorsKeepOnlyTheHost(t *testing.T) {
	got := redactURLs(`Post "https://discord.com/api/v10/interactions/1/secret-token/callback": dial tcp: refused`)
	assert.Equal(t, `Post "https://discord.com/…": dial tcp: refused`, got)
	assert.Equal(t, "see <url> now", redactURLs("see https://%zz now"))
	assert.Equal(t, "no urls here", redactURLs("no urls here"))
}

func TestNonceIsStableBoundedAndPartSensitive(t *testing.T) {
	a := Nonce("task", "turn-1", "0")
	assert.Len(t, a, 24, "Discord caps nonces at 25 characters")
	assert.Equal(t, a, Nonce("task", "turn-1", "0"))
	assert.NotEqual(t, a, Nonce("task", "turn-1", "1"))
	assert.NotEqual(t, Nonce("ab", "c"), Nonce("a", "bc"), "part boundaries matter")
}

func TestRequiredPermissionsAndInvite(t *testing.T) {
	bits := RequiredPermissionBits()
	for _, p := range RequiredPermissions {
		assert.NotZero(t, bits&p.Bit, p.Name)
	}
	assert.Empty(t, MissingPermissions(bits))
	assert.Empty(t, MissingPermissions(Administrator), "Administrator grants everything")
	assert.Equal(t, []string{"MANAGE_ROLES", "PIN_MESSAGES"}, MissingPermissions(bits&^(1<<28)&^(1<<51)))
	assert.Len(t, MissingPermissions(0), len(RequiredPermissions))

	invite := InviteURL("123", "")
	assert.True(t, strings.HasPrefix(invite, "https://discord.com/oauth2/authorize?"))
	assert.Contains(t, invite, "client_id=123")
	assert.Contains(t, invite, "scope=bot+applications.commands")
	assert.NotContains(t, invite, "guild_id")

	pinned := InviteURL("123", "456")
	assert.Contains(t, pinned, "guild_id=456")
	assert.Contains(t, pinned, "disable_guild_select=true")
}

func TestMessageContentIntentFlags(t *testing.T) {
	assert.True(t, MessageContentIntentEnabled(FlagGatewayMessageContent))
	assert.True(t, MessageContentIntentEnabled(FlagGatewayMessageContentLimited))
	assert.False(t, MessageContentIntentEnabled(1<<12))
}
