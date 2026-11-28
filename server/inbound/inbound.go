// Package inbound delivers operators' Discord messages to their Kandev task
// through Messages().Send, which applies Kandev's queue and running-turn
// rules, and acknowledges each with a reaction.
package inbound

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-discord/server/authz"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

// Reactions for each outcome.
const (
	ReactQueued    = "🕒"
	ReactDelivered = "✅"
	ReactFailed    = "⚠️"
	ReactDenied    = "⛔"
)

// Handler delivers messages for one task at a time (the caller serializes).
type Handler struct {
	API  discordapi.API
	Host kandev.Host
	// Save persists the record before and after each delivery.
	Save func(ctx context.Context, rec store.Channel) error
	Now  func() time.Time
	// MaxAge bounds catch-up delivery of messages sent while offline.
	MaxAge time.Duration
	// Authorize returns the author's member record (with roles) and whether
	// they are an operator. An error means authority is unknown (for
	// example Discord was unavailable): the message stays undelivered and
	// the cursor does not move, so a later catch-up retries it.
	Authorize func(ctx context.Context, m *discordgo.Message) (*discordgo.Member, bool, error)
}

// Deliverable reports whether a gateway message is a human chat message the
// bridge should consider: not a bot, webhook, or system message, with text.
func Deliverable(m *discordgo.Message) bool {
	if m == nil || m.Author == nil || m.Author.Bot || m.Author.System || m.WebhookID != "" {
		return false
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return false
	}
	return strings.TrimSpace(m.Content) != ""
}

// Handle processes one message of an active channel and returns the new
// record. A message at or before the inbound cursor was already handled.
func (h *Handler) Handle(ctx context.Context, rec store.Channel, m *discordgo.Message) (store.Channel, error) {
	if rec.InboundCursor != "" && discordapi.CompareIDs(m.ID, rec.InboundCursor) <= 0 {
		return rec, nil
	}
	if rec.InboundInflight == m.ID {
		// A previous process may have delivered it before stopping; never
		// send twice.
		h.reply(ctx, m, ReactFailed, "Delivery of this message is uncertain because the bridge restarted; check the task in Kandev before resending.")
		return h.finish(ctx, rec, m.ID)
	}
	member, operator, err := h.Authorize(ctx, m)
	if err != nil {
		return rec, fmt.Errorf("checking the author's roles: %w", err)
	}
	if !operator {
		h.react(ctx, m, ReactDenied)
		return h.finish(ctx, rec, m.ID)
	}
	rec.InboundInflight = m.ID
	if err := h.Save(ctx, rec); err != nil {
		return rec, fmt.Errorf("saving in-flight marker: %w", err)
	}
	text := authz.DisplayName(member, m.Author) + " (Discord): " + strings.TrimSpace(m.Content)
	dispatch, err := h.Host.SendMessage(ctx, rec.TaskID, text)
	switch {
	case err != nil:
		h.reply(ctx, m, ReactFailed, failureText(err))
	case dispatch != nil && dispatch.Status == "queued":
		h.react(ctx, m, ReactQueued)
	default:
		h.react(ctx, m, ReactDelivered)
	}
	return h.finish(ctx, rec, m.ID)
}

// CatchUp handles messages posted after the inbound cursor while the bridge
// was offline: recent ones go through Handle, older ones are marked as not
// delivered. With until set (a live message's ID), it stops before that
// message: the live message is no missed one, and its own delivery follows.
func (h *Handler) CatchUp(ctx context.Context, rec store.Channel, until string) (store.Channel, error) {
	for {
		batch, err := h.API.ChannelMessages(ctx, rec.ChannelID, 100, rec.InboundCursor)
		if err != nil {
			return rec, fmt.Errorf("reading missed messages: %w", err)
		}
		if len(batch) == 0 {
			return rec, nil
		}
		slices.SortFunc(batch, func(a, b *discordgo.Message) int { return discordapi.CompareIDs(a.ID, b.ID) })
		reached := false
		for _, m := range batch {
			if until != "" && discordapi.CompareIDs(m.ID, until) >= 0 {
				reached = true
				break
			}
			switch {
			case !Deliverable(m):
				rec, err = h.advance(rec, m.ID), nil
			case rec.InboundInflight != m.ID && h.Now().Sub(snowflakeTime(m.ID)) > h.MaxAge:
				rec, err = h.expire(ctx, rec, m)
			default:
				rec, err = h.Handle(ctx, rec, m)
			}
			if err != nil {
				return rec, err
			}
		}
		if err := h.Save(ctx, rec); err != nil {
			return rec, err
		}
		if reached || len(batch) < 100 {
			return rec, nil
		}
	}
}

// expire answers a message too old to deliver: an operator is told to send
// it again, anyone else gets the usual ⛔.
func (h *Handler) expire(ctx context.Context, rec store.Channel, m *discordgo.Message) (store.Channel, error) {
	_, operator, err := h.Authorize(ctx, m)
	if err != nil {
		return rec, fmt.Errorf("checking the author's roles: %w", err)
	}
	if operator {
		h.reply(ctx, m, ReactFailed, "Not delivered — the bridge was offline when this was posted. Send it again if it still applies.")
	} else {
		h.react(ctx, m, ReactDenied)
	}
	return h.finish(ctx, rec, m.ID)
}

func (h *Handler) advance(rec store.Channel, id string) store.Channel {
	if rec.InboundCursor == "" || discordapi.CompareIDs(id, rec.InboundCursor) > 0 {
		rec.InboundCursor = id
	}
	return rec
}

func (h *Handler) finish(ctx context.Context, rec store.Channel, id string) (store.Channel, error) {
	rec = h.advance(rec, id)
	rec.InboundInflight = ""
	return rec, h.Save(ctx, rec)
}

func (h *Handler) react(ctx context.Context, m *discordgo.Message, emoji string) {
	if err := h.API.ReactionAdd(ctx, m.ChannelID, m.ID, emoji); err != nil {
		log.Printf("discord: reacting to message %s: %v", m.ID, err)
	}
}

func (h *Handler) reply(ctx context.Context, m *discordgo.Message, emoji, text string) {
	h.react(ctx, m, emoji)
	_, err := h.API.MessageSend(ctx, m.ChannelID, &discordgo.MessageSend{
		Content:   emoji + " " + text,
		Reference: &discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID, GuildID: m.GuildID},
	})
	if err != nil {
		log.Printf("discord: replying to message %s: %v", m.ID, err)
	}
}

// failureText explains a rejected Send from its gRPC code.
func failureText(err error) string {
	switch status.Code(err) {
	case codes.ResourceExhausted:
		return "Not delivered: this task's message queue is full. Try again after the agent finishes its turn."
	case codes.FailedPrecondition:
		return "Not delivered: the task or its session is no longer active."
	case codes.Aborted:
		return "Not delivered: the task changed or is managed by another owner. Try again."
	case codes.PermissionDenied:
		return "Not delivered: Kandev denied the plugin access to this task."
	case codes.NotFound:
		return "Not delivered: the task no longer exists in Kandev."
	}
	return "Not delivered: Kandev is unavailable. Try again shortly."
}

// snowflakeTime is when Discord created an ID.
func snowflakeTime(id string) time.Time {
	t, err := discordgo.SnowflakeTimestamp(id)
	if err != nil {
		return time.Time{}
	}
	return t
}
