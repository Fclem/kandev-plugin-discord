// Package config parses and validates the operator settings saved on
// Settings > Plugins > Discord (the manifest config_schema), as returned by
// the Host GetConfig RPC.
//
// Validation errors never include the bot token: GetConfig returns secret
// fields in cleartext, and these errors are logged and shown in the UI.
package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Binding maps one Kandev workspace to one Discord guild (server).
type Binding struct {
	WorkspaceID string
	GuildID     string
}

// CloseTrigger selects which tasks lose their open channel.
type CloseTrigger string

// Close triggers.
const (
	CloseOnArchived CloseTrigger = "archived"
	CloseOnTerminal CloseTrigger = "terminal"
)

// ClosedChannelAction selects what happens to a closed task channel.
type ClosedChannelAction string

// Closed channel actions.
const (
	ClosedArchive ClosedChannelAction = "archive"
	ClosedDelete  ClosedChannelAction = "delete"
)

// DefaultOperatorRoleName is the operator role created when none is set.
const DefaultOperatorRoleName = "Kandev Operator"

// Config is the validated plugin configuration. See docs/design/system-design.md
// "Plugin config" for the meaning of each field.
type Config struct {
	BotToken              string
	ApplicationID         string
	Bindings              []Binding
	OperatorRoleName      string
	ViewerRoleName        string
	AllowedUserIDs        []string
	KandevBaseURL         string
	CloseTrigger          CloseTrigger
	ClosedChannelAction   ClosedChannelAction
	ArchiveRetentionDays  int
	MirrorUserMessages    bool
	PostToolActivity      bool
	LiveUpdateInterval    time.Duration
	InboundCatchupMaxAge  time.Duration
	IncludeEphemeralTasks bool
}

var snowflake = regexp.MustCompile(`^[0-9]{17,20}$`)

// IsSnowflake reports whether s looks like a Discord ID.
func IsSnowflake(s string) bool { return snowflake.MatchString(s) }

// Parse validates raw GetConfig output. It returns every problem at once,
// joined, so the settings card can show them together. The returned Config
// carries every field that parsed, even when err is non-nil.
func Parse(raw map[string]any) (Config, error) {
	p := parser{raw: raw}
	cfg := Config{
		BotToken:              strings.TrimSpace(p.str("bot_token")),
		ApplicationID:         strings.TrimSpace(p.str("application_id")),
		OperatorRoleName:      strings.TrimSpace(p.str("operator_role_name")),
		ViewerRoleName:        strings.TrimSpace(p.str("viewer_role_name")),
		CloseTrigger:          CloseTrigger(p.enum("close_trigger", string(CloseOnArchived), string(CloseOnArchived), string(CloseOnTerminal))),
		ClosedChannelAction:   ClosedChannelAction(p.enum("closed_channel_action", string(ClosedArchive), string(ClosedArchive), string(ClosedDelete))),
		ArchiveRetentionDays:  p.nonNegativeInt("archive_retention_days", 30),
		MirrorUserMessages:    p.boolean("mirror_user_messages", true),
		PostToolActivity:      p.boolean("post_tool_activity", false),
		LiveUpdateInterval:    time.Duration(p.nonNegativeInt("live_update_interval_seconds", 30)) * time.Second,
		InboundCatchupMaxAge:  time.Duration(p.nonNegativeInt("inbound_catchup_max_age_minutes", 15)) * time.Minute,
		IncludeEphemeralTasks: p.boolean("include_ephemeral_tasks", false),
	}
	if cfg.OperatorRoleName == "" {
		cfg.OperatorRoleName = DefaultOperatorRoleName
	}
	if cfg.BotToken == "" {
		p.fail(errors.New("bot_token is required"))
	}
	switch {
	case cfg.ApplicationID == "":
		p.fail(errors.New("application_id is required"))
	case !IsSnowflake(cfg.ApplicationID):
		p.fail(errors.New("application_id must be a Discord snowflake (17-20 digits)"))
	}
	if strings.EqualFold(cfg.ViewerRoleName, cfg.OperatorRoleName) {
		p.fail(errors.New("viewer_role_name must differ from operator_role_name"))
	}
	bindings, err := ParseBindings(p.str("workspace_guilds"))
	p.fail(err)
	cfg.Bindings = bindings
	cfg.AllowedUserIDs = p.userIDs("allowed_user_ids")
	cfg.KandevBaseURL = p.baseURL("kandev_base_url")
	return cfg, errors.Join(p.errs...)
}

// ParseBindings parses `<workspace id>=<guild id>` pairs separated by commas
// or newlines. The mapping is one-to-one: a workspace or guild may appear
// only once.
func ParseBindings(raw string) ([]Binding, error) {
	var (
		bindings   []Binding
		errs       []error
		workspaces = map[string]bool{}
		guilds     = map[string]bool{}
	)
	for _, entry := range splitList(raw) {
		workspaceID, guildID, ok := strings.Cut(entry, "=")
		workspaceID, guildID = strings.TrimSpace(workspaceID), strings.TrimSpace(guildID)
		switch {
		case !ok || workspaceID == "" || guildID == "":
			errs = append(errs, fmt.Errorf("workspace_guilds entry %q must look like <workspace id>=<server id>", entry))
			continue
		case strings.ContainsAny(workspaceID, " \t"):
			errs = append(errs, fmt.Errorf("workspace id %q must not contain spaces", workspaceID))
			continue
		case !IsSnowflake(guildID):
			errs = append(errs, fmt.Errorf("server id %q must be a Discord snowflake (17-20 digits)", guildID))
			continue
		case workspaces[workspaceID]:
			errs = append(errs, fmt.Errorf("workspace %s is bound more than once", workspaceID))
			continue
		case guilds[guildID]:
			errs = append(errs, fmt.Errorf("server %s is bound to more than one workspace", guildID))
			continue
		}
		workspaces[workspaceID], guilds[guildID] = true, true
		bindings = append(bindings, Binding{WorkspaceID: workspaceID, GuildID: guildID})
	}
	if len(bindings) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("workspace_guilds needs at least one <workspace id>=<server id> pair"))
	}
	return bindings, errors.Join(errs...)
}

// BindingFor returns the guild bound to workspaceID.
func (c Config) BindingFor(workspaceID string) (Binding, bool) {
	for _, b := range c.Bindings {
		if b.WorkspaceID == workspaceID {
			return b, true
		}
	}
	return Binding{}, false
}

// BindingForGuild returns the workspace bound to guildID.
func (c Config) BindingForGuild(guildID string) (Binding, bool) {
	for _, b := range c.Bindings {
		if b.GuildID == guildID {
			return b, true
		}
	}
	return Binding{}, false
}

// IsAllowedUser reports whether userID is listed in allowed_user_ids.
func (c Config) IsAllowedUser(userID string) bool {
	return slices.Contains(c.AllowedUserIDs, userID)
}

// TaskURL is the Kandev deep link for taskID, or "" without a base URL.
func (c Config) TaskURL(taskID string) string {
	if c.KandevBaseURL == "" || taskID == "" {
		return ""
	}
	return c.KandevBaseURL + "/t/" + url.PathEscape(taskID)
}

type parser struct {
	raw  map[string]any
	errs []error
}

func (p *parser) fail(err error) {
	if err != nil {
		p.errs = append(p.errs, err)
	}
}

func (p *parser) str(key string) string {
	s, _ := p.raw[key].(string)
	return s
}

func (p *parser) enum(key, def string, allowed ...string) string {
	v := strings.TrimSpace(p.str(key))
	if v == "" {
		return def
	}
	if !slices.Contains(allowed, v) {
		p.fail(fmt.Errorf("%s must be %q or %q", key, allowed[0], allowed[1]))
		return def
	}
	return v
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.raw[key]
	if !ok || v == nil {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		p.fail(fmt.Errorf("%s must be true or false", key))
		return def
	}
	return b
}

// nonNegativeInt reads a JSON number (float64 over the wire) that must be a
// whole, non-negative value.
func (p *parser) nonNegativeInt(key string, def int) int {
	v, ok := p.raw[key]
	if !ok || v == nil {
		return def
	}
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		f = -1
	}
	if f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		p.fail(fmt.Errorf("%s must be a non-negative integer", key))
		return def
	}
	return int(f)
}

func (p *parser) userIDs(key string) []string {
	var ids []string
	for _, id := range splitList(p.str(key)) {
		if !IsSnowflake(id) {
			p.fail(fmt.Errorf("%s entry %q must be a Discord user id (17-20 digits)", key, id))
			continue
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *parser) baseURL(key string) string {
	raw := strings.TrimSpace(p.str(key))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		p.fail(fmt.Errorf("%s must be an absolute http(s) URL", key))
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}

func splitList(raw string) []string {
	var out []string
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}
