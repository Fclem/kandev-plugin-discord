package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	appID  = "123456789012345678"
	guildA = "223456789012345678"
	guildB = "323456789012345678"
	token  = "MTIz.secret-token-value"
)

func TestParseBindings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []Binding
		wantErr string
	}{
		{
			name: "newline and comma separated with whitespace",
			raw:  " ws-a = " + guildA + "\r\n,ws-b=" + guildB + "\n",
			want: []Binding{{"ws-a", guildA}, {"ws-b", guildB}},
		},
		{name: "empty", raw: "  \n ", wantErr: "at least one"},
		{name: "missing separator", raw: "ws-a " + guildA, wantErr: "must look like"},
		{name: "non snowflake guild", raw: "ws-a=general", wantErr: "snowflake"},
		{name: "duplicate workspace", raw: "ws-a=" + guildA + "\nws-a=" + guildB, wantErr: "bound more than once"},
		{name: "duplicate guild", raw: "ws-a=" + guildA + "\nws-b=" + guildA, wantErr: "more than one workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBindings(tc.raw)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseBindingsSharedFixture keeps the settings card's parser
// (ui/bundle.js parseBindings) and this one in agreement.
func TestParseBindingsSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/bindings.json")
	require.NoError(t, err)
	var cases []struct {
		Name     string      `json:"name"`
		Input    string      `json:"input"`
		Bindings [][2]string `json:"bindings"`
		Problems int         `json:"problems"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	for _, c := range cases {
		got, err := ParseBindings(c.Input)
		pairs := [][2]string{}
		for _, b := range got {
			pairs = append(pairs, [2]string{b.WorkspaceID, b.GuildID})
		}
		assert.Equal(t, c.Bindings, pairs, c.Name)
		problems := 0
		if err != nil {
			problems = len(err.(interface{ Unwrap() []error }).Unwrap())
		}
		assert.Equal(t, c.Problems, problems, c.Name)
	}
}

func TestParseReportsAllProblemsWithoutLeakingToken(t *testing.T) {
	_, err := Parse(map[string]any{
		"bot_token":        token,
		"application_id":   "not-an-id",
		"workspace_guilds": "ws-a=nope",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "application_id must be a Discord snowflake")
	assert.Contains(t, err.Error(), `server id "nope"`)
	assert.False(t, strings.Contains(err.Error(), token), "error must not echo the bot token")
}

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse(map[string]any{
		"bot_token":        "  " + token + " ",
		"application_id":   appID,
		"workspace_guilds": "ws-a=" + guildA,
	})
	require.NoError(t, err)
	assert.Equal(t, token, cfg.BotToken)
	b, ok := cfg.BindingFor("ws-a")
	require.True(t, ok)
	assert.Equal(t, guildA, b.GuildID)
	_, ok = cfg.BindingFor("ws-z")
	assert.False(t, ok)
}

func TestParseMissingRequired(t *testing.T) {
	_, err := Parse(map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bot_token is required")
	assert.Contains(t, err.Error(), "application_id is required")
	assert.Contains(t, err.Error(), "workspace_guilds needs at least one")
}

func validRaw() map[string]any {
	return map[string]any{
		"bot_token":        token,
		"application_id":   appID,
		"workspace_guilds": "ws-a=" + guildA,
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(validRaw())
	require.NoError(t, err)
	assert.Equal(t, "Kandev Operator", cfg.OperatorRoleName)
	assert.Empty(t, cfg.ViewerRoleName)
	assert.Empty(t, cfg.AllowedUserIDs)
	assert.Equal(t, CloseOnArchived, cfg.CloseTrigger)
	assert.Equal(t, ClosedArchive, cfg.ClosedChannelAction)
	assert.Equal(t, 30, cfg.ArchiveRetentionDays)
	assert.True(t, cfg.MirrorUserMessages)
	assert.False(t, cfg.PostToolActivity)
	assert.Equal(t, 30*time.Second, cfg.LiveUpdateInterval)
	assert.Equal(t, 15*time.Minute, cfg.InboundCatchupMaxAge)
	assert.False(t, cfg.IncludeEphemeralTasks)
}

func TestParseOptionalFields(t *testing.T) {
	raw := validRaw()
	raw["operator_role_name"] = " Ops "
	raw["viewer_role_name"] = "Watchers"
	raw["allowed_user_ids"] = guildB + ", " + appID
	raw["kandev_base_url"] = "https://kandev.example.com/"
	raw["close_trigger"] = "terminal"
	raw["closed_channel_action"] = "delete"
	raw["archive_retention_days"] = float64(0)
	raw["mirror_user_messages"] = false
	raw["post_tool_activity"] = true
	raw["live_update_interval_seconds"] = float64(0)
	raw["inbound_catchup_max_age_minutes"] = float64(5)
	raw["include_ephemeral_tasks"] = true
	cfg, err := Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "Ops", cfg.OperatorRoleName)
	assert.Equal(t, "Watchers", cfg.ViewerRoleName)
	assert.Equal(t, []string{guildB, appID}, cfg.AllowedUserIDs)
	assert.Equal(t, "https://kandev.example.com", cfg.KandevBaseURL)
	assert.Equal(t, CloseOnTerminal, cfg.CloseTrigger)
	assert.Equal(t, ClosedDelete, cfg.ClosedChannelAction)
	assert.Equal(t, 0, cfg.ArchiveRetentionDays)
	assert.False(t, cfg.MirrorUserMessages)
	assert.True(t, cfg.PostToolActivity)
	assert.Equal(t, time.Duration(0), cfg.LiveUpdateInterval)
	assert.Equal(t, 5*time.Minute, cfg.InboundCatchupMaxAge)
	assert.True(t, cfg.IncludeEphemeralTasks)
}

func TestParseRejectsInvalidOptionalFields(t *testing.T) {
	raw := validRaw()
	raw["allowed_user_ids"] = "alice"
	raw["kandev_base_url"] = "kandev.local"
	raw["close_trigger"] = "never"
	raw["closed_channel_action"] = "burn"
	raw["archive_retention_days"] = float64(-1)
	raw["live_update_interval_seconds"] = 2.5
	_, err := Parse(raw)
	require.Error(t, err)
	for _, want := range []string{
		`allowed_user_ids entry "alice"`,
		"kandev_base_url must be an absolute http(s) URL",
		`close_trigger must be "archived" or "terminal"`,
		`closed_channel_action must be "archive" or "delete"`,
		"archive_retention_days must be a non-negative integer",
		"live_update_interval_seconds must be a non-negative integer",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestIsOperatorUser(t *testing.T) {
	raw := validRaw()
	raw["allowed_user_ids"] = guildB
	cfg, err := Parse(raw)
	require.NoError(t, err)
	assert.True(t, cfg.IsAllowedUser(guildB))
	assert.False(t, cfg.IsAllowedUser(appID))
	b, ok := cfg.BindingForGuild(guildA)
	require.True(t, ok)
	assert.Equal(t, "ws-a", b.WorkspaceID)
}
