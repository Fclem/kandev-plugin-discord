// Package discord is a minimal Discord REST v10 client covering the calls the
// plugin makes today. The gateway connection and the full API seam land with
// the channel features (docs/design/plan.md, task-05/06).
package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the Discord REST API root, version 10.
const DefaultBaseURL = "https://discord.com/api/v10"

const userAgent = "DiscordBot (https://github.com/Fclem/kandev-plugin-discord, 0.1.0)"

// Client calls the Discord REST API as a bot. It never logs or returns the
// token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient returns a client for the public Discord API.
func NewClient(token string) *Client {
	return &Client{BaseURL: DefaultBaseURL, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// User is the subset of the Discord user object the plugin reads.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// Application is the subset of the Discord application object the plugin
// reads.
type Application struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Flags uint64 `json:"flags"`
}

// PartialGuild is an entry of GET /users/@me/guilds. Permissions is the
// bot's computed guild permission bitfield, as a decimal string.
type PartialGuild struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Permissions string `json:"permissions"`
}

// APIError is a non-2xx Discord response.
type APIError struct {
	Status     int
	Code       int
	Message    string
	RetryAfter float64
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("discord API returned HTTP %d", e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Status == http.StatusTooManyRequests && e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %.1fs)", e.RetryAfter)
	}
	return msg
}

// CurrentUser calls GET /users/@me and validates the token.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	return u, c.get(ctx, "/users/@me", &u)
}

// CurrentApplication calls GET /applications/@me. Flags carry the
// privileged-intent toggles from the Developer Portal.
func (c *Client) CurrentApplication(ctx context.Context) (Application, error) {
	var a Application
	return a, c.get(ctx, "/applications/@me", &a)
}

// CurrentUserGuilds calls GET /users/@me/guilds. It returns at most 200
// guilds, which covers a bot dedicated to one Kandev instance.
func (c *Client) CurrentUserGuilds(ctx context.Context) ([]PartialGuild, error) {
	var gs []PartialGuild
	return gs, c.get(ctx, "/users/@me/guilds", &gs)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.Token)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// net/http errors include the URL, never the Authorization header.
		return fmt.Errorf("calling discord %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading discord %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		var payload struct {
			Code       int     `json:"code"`
			Message    string  `json:"message"`
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(body, &payload) == nil {
			apiErr.Code, apiErr.Message, apiErr.RetryAfter = payload.Code, payload.Message, payload.RetryAfter
		}
		return apiErr
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding discord %s response: %w", path, err)
	}
	return nil
}
