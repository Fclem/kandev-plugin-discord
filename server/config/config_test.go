package config

import (
	"strings"
	"testing"

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
