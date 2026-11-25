// Package health implements the settings card's connection test: it checks
// the bot token, the application and its Message Content intent, and the
// bot's membership and permissions in the server bound to one workspace.
package health

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/store"
)

// Check statuses.
const (
	Pass = "pass"
	Fail = "fail"
	Skip = "skip"
)

// Check IDs, stable for the UI.
const (
	CheckConfig      = "config"
	CheckToken       = "token"
	CheckApplication = "application"
	CheckIntent      = "message_content_intent"
	CheckMembership  = "server_membership"
	CheckPermissions = "permissions"
)

// Check is one line of the connection test report.
type Check struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Result is the connection test report for one workspace. It never contains
// the bot token.
type Result struct {
	OK          bool    `json:"ok"`
	WorkspaceID string  `json:"workspace_id"`
	GuildID     string  `json:"guild_id,omitempty"`
	InviteURL   string  `json:"invite_url,omitempty"`
	Checks      []Check `json:"checks"`
	// Gateway is the bridge's cached state (connection.status); it does not
	// affect OK, which covers the live checks only.
	Gateway GatewayStatus `json:"gateway"`
}

// GatewayStatus is the cached bridge state for one workspace, served by
// connection.status without calling Discord.
type GatewayStatus struct {
	State         string `json:"state"`
	Detail        string `json:"detail,omitempty"`
	CloseCode     int    `json:"close_code,omitempty"`
	Identifies24h int    `json:"identifies_24h"`
	ConfigError   string `json:"config_error,omitempty"`
	BotMember     bool   `json:"bot_member"`
	Provisioned   bool   `json:"provisioned"`
	InviteURL     string `json:"invite_url,omitempty"`
	Error         string `json:"error,omitempty"`
	CheckedAt     string `json:"checked_at,omitempty"`
}

// StateUnknown means the bridge has not reported yet.
const StateUnknown = "unknown"

// Status projects the cached health record onto one workspace.
func Status(h store.Health, workspaceID string) GatewayStatus {
	s := GatewayStatus{
		State: h.GatewayState, Detail: h.Reason, CloseCode: h.LastCloseCode,
		Identifies24h: h.IdentifyCount24h, ConfigError: h.ConfigError,
	}
	if s.State == "" {
		s.State = StateUnknown
	}
	if !h.CheckedAt.IsZero() {
		s.CheckedAt = h.CheckedAt.UTC().Format(time.RFC3339)
	}
	for _, b := range h.Bindings {
		if b.WorkspaceID == workspaceID {
			s.BotMember, s.Provisioned, s.InviteURL, s.Error = b.BotMember, b.Provisioned, b.InviteURL, b.Error
		}
	}
	return s
}

// API is the Discord surface the test needs; discordapi.API satisfies it.
type API interface {
	CurrentUser(ctx context.Context) (*discordgo.User, error)
	CurrentApplication(ctx context.Context) (*discordgo.Application, error)
	CurrentUserGuilds(ctx context.Context) ([]*discordgo.UserGuild, error)
}

// Test runs every check that its prerequisites allow; later checks are
// skipped, not failed, when an earlier one makes them meaningless.
func Test(ctx context.Context, api API, cfg config.Config, cfgErr error, workspaceID string) Result {
	r := &report{res: Result{WorkspaceID: workspaceID}}
	binding, bound := cfg.BindingFor(workspaceID)
	switch {
	case cfg.BotToken == "":
		r.add(CheckConfig, Fail, configDetail("bot_token is required", cfgErr))
		r.skipRest(CheckToken, CheckApplication, CheckIntent, CheckMembership, CheckPermissions)
		return r.done()
	case !bound:
		r.add(CheckConfig, Fail, configDetail("no Discord server is bound to this workspace in workspace_guilds", cfgErr))
	case cfgErr != nil:
		r.add(CheckConfig, Fail, cfgErr.Error())
	default:
		r.res.GuildID = binding.GuildID
		r.add(CheckConfig, Pass, "server "+binding.GuildID+" is bound to this workspace")
	}

	user, err := api.CurrentUser(ctx)
	if err != nil {
		r.add(CheckToken, Fail, tokenDetail(err))
		r.skipRest(CheckApplication, CheckIntent, CheckMembership, CheckPermissions)
		return r.done()
	}
	r.add(CheckToken, Pass, "authenticated as "+user.Username)

	r.checkApplication(ctx, api, cfg)
	if !bound {
		r.skipRest(CheckMembership, CheckPermissions)
		return r.done()
	}
	r.checkGuild(ctx, api, cfg, binding)
	return r.done()
}

type report struct{ res Result }

func (r *report) add(id, status, detail string) {
	r.res.Checks = append(r.res.Checks, Check{ID: id, Status: status, Detail: detail})
}

func (r *report) skipRest(ids ...string) {
	for _, id := range ids {
		r.add(id, Skip, "skipped: an earlier check failed")
	}
}

func (r *report) done() Result {
	r.res.OK = true
	for _, c := range r.res.Checks {
		if c.Status != Pass {
			r.res.OK = false
		}
	}
	return r.res
}

func (r *report) checkApplication(ctx context.Context, api API, cfg config.Config) {
	app, err := api.CurrentApplication(ctx)
	if err != nil {
		r.add(CheckApplication, Fail, err.Error())
		r.add(CheckIntent, Skip, "skipped: application could not be read")
		return
	}
	if cfg.ApplicationID != "" && app.ID != cfg.ApplicationID {
		r.add(CheckApplication, Fail, fmt.Sprintf("the token belongs to application %s, but application_id is %s", app.ID, cfg.ApplicationID))
	} else {
		r.add(CheckApplication, Pass, "application "+app.Name)
	}
	if discordapi.MessageContentIntentEnabled(uint64(app.Flags)) {
		r.add(CheckIntent, Pass, "Message Content intent is enabled")
	} else {
		r.add(CheckIntent, Fail, "Message Content intent is disabled: Developer Portal > Bot > Privileged Gateway Intents > Message Content Intent")
	}
}

func (r *report) checkGuild(ctx context.Context, api API, cfg config.Config, binding config.Binding) {
	guilds, err := api.CurrentUserGuilds(ctx)
	if err != nil {
		r.add(CheckMembership, Fail, err.Error())
		r.add(CheckPermissions, Skip, "skipped: server list could not be read")
		return
	}
	var guild *discordgo.UserGuild
	for _, g := range guilds {
		if g.ID == binding.GuildID {
			guild = g
		}
	}
	if guild == nil {
		r.add(CheckMembership, Fail, "the bot is not a member of server "+binding.GuildID+"; invite it with the link below")
		r.add(CheckPermissions, Skip, "skipped: the bot is not in the server")
		r.setInvite(cfg, binding)
		return
	}
	r.add(CheckMembership, Pass, "member of "+guild.Name)
	granted := uint64(guild.Permissions)
	if missing := discordapi.MissingPermissions(granted); len(missing) > 0 {
		r.add(CheckPermissions, Fail, "missing: "+strings.Join(missing, ", ")+"; re-invite with the link below or grant them to the bot's role")
		r.setInvite(cfg, binding)
		return
	}
	r.add(CheckPermissions, Pass, "all required permissions granted")
}

func (r *report) setInvite(cfg config.Config, binding config.Binding) {
	if cfg.ApplicationID != "" {
		r.res.InviteURL = discordapi.InviteURL(cfg.ApplicationID, binding.GuildID)
	}
}

func configDetail(primary string, cfgErr error) string {
	if cfgErr == nil {
		return primary
	}
	return primary + "; " + cfgErr.Error()
}

func tokenDetail(err error) string {
	if discordapi.Status(err) == http.StatusUnauthorized {
		return "Discord rejected the bot token (401): reset it on the Bot page and save it again"
	}
	return err.Error()
}
