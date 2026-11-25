package outbound

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

// CardDebounce is the minimum time between two task card edits.
const CardDebounce = 10 * time.Second

// Mirror posts one task's Kandev activity to its channel.
type Mirror struct {
	API  discordapi.API
	Host kandev.Host
	Opts Options
	// Save persists the record after each posted message, so a crash never
	// reposts what Discord already shows.
	Save func(ctx context.Context, rec store.Channel) error
	// TaskURL is the Kandev deep link for a task, or "".
	TaskURL func(taskID string) string
}

// Input is one task's observed activity.
type Input struct {
	Task     *pluginsdk.Task
	Rec      store.Channel
	Sessions []pluginsdk.Session
	Card     CardInput
	Now      time.Time
}

// Sync posts new messages, status lines, and the card. It returns the new
// record and, when the card edit was debounced, when to sync again.
func (m *Mirror) Sync(ctx context.Context, in Input) (store.Channel, time.Duration, error) {
	rec := in.Rec
	if err := m.pull(ctx, in.Task, &rec, in.Sessions); err != nil {
		return rec, 0, err
	}
	lines, next := StatusLines(rec.SessionStates, in.Sessions)
	for _, line := range lines {
		if _, err := m.API.MessageSend(ctx, rec.ChannelID, &discordgo.MessageSend{Content: line}); err != nil {
			return rec, 0, fmt.Errorf("posting status line: %w", err)
		}
	}
	if !maps.Equal(next, rec.SessionStates) {
		rec.SessionStates = next
	}
	retry, err := m.card(ctx, &rec, in.Card, in.Now)
	return rec, retry, err
}

// pull posts messages created after the cursor, oldest first. While a
// session runs, its newest agent message may still be growing, so mirroring
// stops before it until the turn ends.
func (m *Mirror) pull(ctx context.Context, task *pluginsdk.Task, rec *store.Channel, sessions []pluginsdk.Session) error {
	msgs, err := m.Host.ListMessages(ctx, task.ID, rec.OutboundCursor.CreatedAt)
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}
	sort.SliceStable(msgs, func(i, j int) bool { return parseTime(msgs[i].CreatedAt).Before(parseTime(msgs[j].CreatedAt)) })
	held := heldBack(msgs, sessions)
	cursorAt := parseTime(rec.OutboundCursor.CreatedAt)
	for _, msg := range msgs {
		at := parseTime(msg.CreatedAt)
		if at.Before(cursorAt) || (at.Equal(cursorAt) && slices.Contains(rec.OutboundCursor.IDs, msg.ID)) {
			continue
		}
		if held[msg.ID] {
			return nil
		}
		text, post := Render(msg, m.Opts)
		if post {
			if err := m.post(ctx, rec.ChannelID, text, m.TaskURL(task.ID)); err != nil {
				return err
			}
		}
		if at.Equal(cursorAt) {
			rec.OutboundCursor.IDs = append(rec.OutboundCursor.IDs, msg.ID)
		} else {
			rec.OutboundCursor = store.Cursor{CreatedAt: msg.CreatedAt, IDs: []string{msg.ID}}
			cursorAt = at
		}
		if post && m.Save != nil {
			if err := m.Save(ctx, *rec); err != nil {
				return fmt.Errorf("saving cursor: %w", err)
			}
		}
	}
	return nil
}

// post sends one rendered message as up to five chunks, then a truncation
// line.
func (m *Mirror) post(ctx context.Context, channelID, text, url string) error {
	chunks, truncated := Limit(Chunk(text, MaxMessageLength))
	for _, c := range chunks {
		if _, err := m.API.MessageSend(ctx, channelID, &discordgo.MessageSend{Content: c}); err != nil {
			return fmt.Errorf("posting message: %w", err)
		}
	}
	if truncated {
		line := "… truncated — open the task in Kandev for the full message."
		if url != "" {
			line = "… truncated — open in Kandev: " + url
		}
		if _, err := m.API.MessageSend(ctx, channelID, &discordgo.MessageSend{Content: line}); err != nil {
			return fmt.Errorf("posting truncation line: %w", err)
		}
	}
	return nil
}

// heldBack marks the newest agent message of each running session.
func heldBack(msgs []pluginsdk.Message, sessions []pluginsdk.Session) map[string]bool {
	running := map[string]bool{}
	for _, s := range sessions {
		if Running(s.State) {
			running[s.ID] = true
		}
	}
	newest := map[string]string{}
	for _, msg := range msgs {
		if msg.AuthorType == "agent" && running[msg.SessionID] {
			newest[msg.SessionID] = msg.ID
		}
	}
	held := map[string]bool{}
	for _, id := range newest {
		held[id] = true
	}
	return held
}

// card sends and pins the task card once, then edits it when its content
// changes, at most every CardDebounce.
func (m *Mirror) card(ctx context.Context, rec *store.Channel, in CardInput, now time.Time) (time.Duration, error) {
	embed, hash := Card(in)
	if rec.CardMessageID != "" && rec.CardHash == hash {
		return 0, nil
	}
	if rec.CardMessageID != "" {
		if wait := CardDebounce - now.Sub(rec.CardEditedAt); wait > 0 {
			return wait, nil
		}
		embeds := []*discordgo.MessageEmbed{embed}
		_, err := m.API.MessageEdit(ctx, &discordgo.MessageEdit{Channel: rec.ChannelID, ID: rec.CardMessageID, Embeds: &embeds})
		if err == nil {
			rec.CardHash, rec.CardEditedAt = hash, now
			return 0, nil
		}
		if !discordapi.IsGone(err) {
			return 0, fmt.Errorf("editing task card: %w", err)
		}
	}
	msg, err := m.API.MessageSend(ctx, rec.ChannelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}})
	if err != nil {
		return 0, fmt.Errorf("posting task card: %w", err)
	}
	rec.CardMessageID, rec.CardHash, rec.CardEditedAt = msg.ID, hash, now
	if err := m.API.MessagePin(ctx, rec.ChannelID, msg.ID); err != nil && !discordapi.HasCode(err, discordapi.CodeMaxPinsReached) {
		return 0, fmt.Errorf("pinning task card: %w", err)
	}
	return 0, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
