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
	"regexp"
	"strings"
)

// Binding maps one Kandev workspace to one Discord guild (server).
type Binding struct {
	WorkspaceID string
	GuildID     string
}

// Config is the subset of settings the plugin currently uses. Fields are added
// as features land; see docs/design/spec.md for the full schema.
type Config struct {
	BotToken      string
	ApplicationID string
	Bindings      []Binding
}

var snowflake = regexp.MustCompile(`^[0-9]{17,20}$`)

// Parse validates raw GetConfig output. It returns every problem at once,
// joined, so the settings card can show them together.
func Parse(raw map[string]any) (Config, error) {
	cfg := Config{
		BotToken:      strings.TrimSpace(stringValue(raw, "bot_token")),
		ApplicationID: strings.TrimSpace(stringValue(raw, "application_id")),
	}
	var errs []error
	if cfg.BotToken == "" {
		errs = append(errs, errors.New("bot_token is required"))
	}
	switch {
	case cfg.ApplicationID == "":
		errs = append(errs, errors.New("application_id is required"))
	case !snowflake.MatchString(cfg.ApplicationID):
		errs = append(errs, errors.New("application_id must be a Discord snowflake (17-20 digits)"))
	}
	bindings, err := ParseBindings(stringValue(raw, "workspace_guilds"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.Bindings = bindings
	return cfg, errors.Join(errs...)
}

// ParseBindings parses `<workspace id>=<guild id>` pairs separated by commas
// or newlines. The mapping is one-to-one: a workspace or guild may appear
// only once.
func ParseBindings(raw string) ([]Binding, error) {
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	var (
		bindings   []Binding
		errs       []error
		workspaces = map[string]bool{}
		guilds     = map[string]bool{}
	)
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		workspaceID, guildID, ok := strings.Cut(entry, "=")
		workspaceID, guildID = strings.TrimSpace(workspaceID), strings.TrimSpace(guildID)
		switch {
		case !ok || workspaceID == "" || guildID == "":
			errs = append(errs, fmt.Errorf("workspace_guilds entry %q must look like <workspace id>=<server id>", entry))
			continue
		case strings.ContainsAny(workspaceID, " \t"):
			errs = append(errs, fmt.Errorf("workspace id %q must not contain spaces", workspaceID))
			continue
		case !snowflake.MatchString(guildID):
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

func stringValue(raw map[string]any, key string) string {
	s, _ := raw[key].(string)
	return s
}
