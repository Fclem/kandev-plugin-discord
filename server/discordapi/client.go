package discordapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

// GlobalRequestsPerSecond caps all REST calls, below Discord's global 50/s.
const GlobalRequestsPerSecond = 40

// Client implements API over a discordgo session. The same session drives
// the gateway; discordgo applies per-route buckets and 429 retry_after.
type Client struct {
	s       *discordgo.Session
	halted  atomic.Bool
	limiter *limiter
}

var _ API = (*Client)(nil)

// NewSession returns a discordgo session for a bot token with REST defaults
// suitable for the plugin. It does not open the gateway.
func NewSession(token string) *discordgo.Session {
	s, _ := discordgo.New("Bot " + token) // only fails for an empty token with login, never here
	s.Client = &http.Client{Timeout: 20 * time.Second}
	s.UserAgent = "DiscordBot (https://github.com/Fclem/kandev-plugin-discord, 0.1.0)"
	s.ShouldRetryOnRateLimit = true
	s.MaxRestRetries = 2
	s.LogLevel = discordgo.LogError
	return s
}

// NewClient wraps s.
func NewClient(s *discordgo.Session) *Client {
	return &Client{s: s, limiter: newLimiter(GlobalRequestsPerSecond)}
}

// Halted reports whether a 401 stopped all bot calls.
func (c *Client) Halted() bool { return c.halted.Load() }

// do runs one bot-authenticated call through the halt flag and the global
// budget, and maps its error.
func (c *Client) do(ctx context.Context, call func(opt discordgo.RequestOption) error) error {
	if c.halted.Load() {
		return ErrHalted
	}
	if err := c.limiter.wait(ctx); err != nil {
		return err
	}
	err := mapError(call(discordgo.WithContext(ctx)))
	if Status(err) == http.StatusUnauthorized {
		c.halted.Store(true)
		return errors.Join(ErrHalted, err)
	}
	return err
}

// doInteraction runs a call authenticated by an interaction token. Its 401
// means an expired token, not a revoked bot token, so it never halts.
func (c *Client) doInteraction(ctx context.Context, call func(opt discordgo.RequestOption) error) error {
	if err := c.limiter.wait(ctx); err != nil {
		return err
	}
	return mapError(call(discordgo.WithContext(ctx)))
}

func (c *Client) CurrentUser(ctx context.Context) (u *discordgo.User, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { u, err = c.s.User("@me", o); return err })
	return u, err
}

func (c *Client) CurrentApplication(ctx context.Context) (app *discordgo.Application, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error {
		body, err := c.s.RequestWithBucketID(http.MethodGet, discordgo.EndpointApplications+"/@me", nil, discordgo.EndpointApplications+"/@me", o)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, &app)
	})
	return app, err
}

func (c *Client) CurrentUserGuilds(ctx context.Context) (gs []*discordgo.UserGuild, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { gs, err = c.s.UserGuilds(200, "", "", false, o); return err })
	return gs, err
}

func (c *Client) GuildMember(ctx context.Context, guildID, userID string) (m *discordgo.Member, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { m, err = c.s.GuildMember(guildID, userID, o); return err })
	return m, err
}

func (c *Client) GuildRoles(ctx context.Context, guildID string) (rs []*discordgo.Role, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { rs, err = c.s.GuildRoles(guildID, o); return err })
	return rs, err
}

func (c *Client) GuildRoleCreate(ctx context.Context, guildID string, params *discordgo.RoleParams) (r *discordgo.Role, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { r, err = c.s.GuildRoleCreate(guildID, params, o); return err })
	return r, err
}

func (c *Client) GuildChannels(ctx context.Context, guildID string) (cs []*discordgo.Channel, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { cs, err = c.s.GuildChannels(guildID, o); return err })
	return cs, err
}

func (c *Client) GuildChannelCreate(ctx context.Context, guildID string, data discordgo.GuildChannelCreateData) (ch *discordgo.Channel, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error {
		ch, err = c.s.GuildChannelCreateComplex(guildID, data, o)
		return err
	})
	return ch, err
}

func (c *Client) ChannelEdit(ctx context.Context, channelID string, data *discordgo.ChannelEdit) (ch *discordgo.Channel, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error { ch, err = c.s.ChannelEdit(channelID, data, o); return err })
	return ch, err
}

func (c *Client) ChannelDelete(ctx context.Context, channelID string) error {
	return c.do(ctx, func(o discordgo.RequestOption) error { _, err := c.s.ChannelDelete(channelID, o); return err })
}

func (c *Client) ChannelMessages(ctx context.Context, channelID string, limit int, afterID string) (ms []*discordgo.Message, err error) {
	err = c.do(ctx, func(o discordgo.RequestOption) error {
		ms, err = c.s.ChannelMessages(channelID, limit, "", afterID, "", o)
		return err
	})
	return ms, err
}

func (c *Client) MessageSend(ctx context.Context, channelID string, data *discordgo.MessageSend) (m *discordgo.Message, err error) {
	data.AllowedMentions = NoMentions()
	err = c.do(ctx, func(o discordgo.RequestOption) error {
		m, err = c.s.ChannelMessageSendComplex(channelID, data, o)
		return err
	})
	return m, err
}

func (c *Client) MessageEdit(ctx context.Context, data *discordgo.MessageEdit) (m *discordgo.Message, err error) {
	data.AllowedMentions = NoMentions()
	err = c.do(ctx, func(o discordgo.RequestOption) error { m, err = c.s.ChannelMessageEditComplex(data, o); return err })
	return m, err
}

func (c *Client) ReactionAdd(ctx context.Context, channelID, messageID, emoji string) error {
	return c.do(ctx, func(o discordgo.RequestOption) error { return c.s.MessageReactionAdd(channelID, messageID, emoji, o) })
}

func (c *Client) MessagePin(ctx context.Context, channelID, messageID string) error {
	return c.do(ctx, func(o discordgo.RequestOption) error { return c.s.ChannelMessagePin(channelID, messageID, o) })
}

func (c *Client) BulkOverwriteGuildCommands(ctx context.Context, appID, guildID string, commands []*discordgo.ApplicationCommand) error {
	return c.do(ctx, func(o discordgo.RequestOption) error {
		_, err := c.s.ApplicationCommandBulkOverwrite(appID, guildID, commands, o)
		return err
	})
}

func (c *Client) InteractionRespond(ctx context.Context, interaction *discordgo.Interaction, resp *discordgo.InteractionResponse) error {
	if resp.Data != nil {
		resp.Data.AllowedMentions = NoMentions()
	}
	return c.doInteraction(ctx, func(o discordgo.RequestOption) error { return c.s.InteractionRespond(interaction, resp, o) })
}

func (c *Client) InteractionEdit(ctx context.Context, interaction *discordgo.Interaction, edit *discordgo.WebhookEdit) error {
	edit.AllowedMentions = NoMentions()
	return c.doInteraction(ctx, func(o discordgo.RequestOption) error {
		_, err := c.s.InteractionResponseEdit(interaction, edit, o)
		return err
	})
}

// mapError converts discordgo REST errors into *Error, dropping the request
// (which carries the Authorization header) and the raw body.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil {
		out := &Error{Status: rest.Response.StatusCode}
		if rest.Message != nil {
			out.Code, out.Message = rest.Message.Code, rest.Message.Message
		}
		return out
	}
	return err
}

// limiter spaces calls evenly at a fixed rate.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(perSecond int) *limiter {
	return &limiter{interval: time.Second / time.Duration(perSecond)}
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	delay := l.next.Sub(now)
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
