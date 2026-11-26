package outbound

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
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

// Result is one Sync's outcome.
type Result struct {
	Rec store.Channel
	// Retry is when to sync again: a debounced card edit, or a held
	// message whose session already stopped.
	Retry time.Duration
	// Held reports a message left for a later sync because its session
	// was, or may have been, still writing it.
	Held bool
}

// heldRecheck is how soon a message held only because its session changed
// state during the sync is looked at again.
const heldRecheck = 5 * time.Second

// Sync posts new messages, status lines, and the card. A card failure is
// returned as a *CardError: messages and status lines were flushed.
func (m *Mirror) Sync(ctx context.Context, in Input) (Result, error) {
	res := Result{Rec: in.Rec}
	rec := &res.Rec
	held, recheck, err := m.pull(ctx, in.Task, rec, in.Sessions)
	res.Held = held
	if err != nil {
		return res, err
	}
	lines, next := StatusLines(rec.SessionStates, in.Sessions)
	for _, line := range lines {
		if _, err := m.API.MessageSend(ctx, rec.ChannelID, &discordgo.MessageSend{Content: line}); err != nil {
			return res, fmt.Errorf("posting status line: %w", err)
		}
	}
	if !maps.Equal(next, rec.SessionStates) {
		rec.SessionStates = next
	}
	retry, err := m.card(ctx, rec, in.Card, in.Now)
	if recheck && (retry == 0 || retry > heldRecheck) {
		retry = heldRecheck
	}
	res.Retry = retry
	if err != nil {
		return res, &CardError{Err: err}
	}
	return res, nil
}

// CardError is a task card failure; the card is cosmetic and never holds
// back a channel close.
type CardError struct{ Err error }

func (e *CardError) Error() string { return e.Err.Error() }
func (e *CardError) Unwrap() error { return e.Err }

// pull posts messages created after the cursor, oldest first. While a
// session runs, its newest agent message may still be growing, so mirroring
// stops before it until the turn ends. Sessions are read before and after
// the messages: a session running (or changing state) in either read may
// have been streaming when its message was read. It reports whether it
// stopped at such a message, and whether to look again soon because no
// session runs any more (no later event may come).
//
// The Host filters by the precise cursor but serializes CreatedAt in whole
// seconds, so returned rows are compared with the cursor at that precision;
// the ID tie-break separates rows within one second.
func (m *Mirror) pull(ctx context.Context, task *pluginsdk.Task, rec *store.Channel, sessions []pluginsdk.Session) (held, recheck bool, err error) {
	msgs, err := m.Host.ListMessages(ctx, task.ID, rec.OutboundCursor.CreatedAt)
	if err != nil {
		return false, false, fmt.Errorf("listing messages: %w", err)
	}
	after, err := m.Host.ListSessions(ctx, task.ID)
	if err != nil {
		return false, false, fmt.Errorf("listing sessions: %w", err)
	}
	sort.SliceStable(msgs, func(i, j int) bool { return parseTime(msgs[i].CreatedAt).Before(parseTime(msgs[j].CreatedAt)) })
	holds := heldBack(msgs, sessions, after)
	c := &rec.OutboundCursor
	cursorAt := second(c.CreatedAt)
	for _, msg := range msgs {
		at := second(msg.CreatedAt)
		if at.Before(cursorAt) || (at.Equal(cursorAt) && slices.Contains(c.IDs, msg.ID)) {
			continue
		}
		if holds[msg.ID] {
			return true, !slices.ContainsFunc(after, func(s pluginsdk.Session) bool { return Running(s.State) }), nil
		}
		text, post := Render(msg, m.Opts)
		if post {
			if err := m.post(ctx, rec, msg.ID, Parts(text, m.TaskURL(task.ID))); err != nil {
				return false, false, err
			}
		}
		if at.Equal(cursorAt) {
			c.IDs = append(c.IDs, msg.ID)
		} else {
			c.CreatedAt, c.IDs, cursorAt = msg.CreatedAt, []string{msg.ID}, at
		}
		c.PartialID, c.PartialChannelID, c.PartsSent = "", "", 0
		if post {
			if err := m.save(ctx, *rec); err != nil {
				return false, false, fmt.Errorf("saving cursor: %w", err)
			}
		}
	}
	return false, false, nil
}

// Parts renders one message as up to five chunks plus, when it was cut, a
// truncation line.
func Parts(text, url string) []string {
	chunks, truncated := Limit(Chunk(text, MaxMessageLength))
	if truncated {
		line := "… truncated — open the task in Kandev for the full message."
		if url != "" {
			line = "… truncated — open in Kandev: " + url
		}
		chunks = append(chunks, line)
	}
	return chunks
}

// post sends one message's parts to the record's channel, counting each
// delivered part in the record (saved), so a retry resumes after them.
// Each part carries a nonce derived from the channel, message, and part
// and Discord enforces it: a part whose reply was lost (or whose count was
// not saved before a restart) is not posted again when retried within
// Discord's nonce window. Progress made for another channel is discarded:
// a replacement channel gets the whole message. The caller saves after the
// last part, together with the cursor.
func (m *Mirror) post(ctx context.Context, rec *store.Channel, msgID string, parts []string) error {
	c := &rec.OutboundCursor
	if c.PartialID != msgID || c.PartialChannelID != rec.ChannelID {
		c.PartialID, c.PartialChannelID, c.PartsSent = msgID, rec.ChannelID, 0
	}
	for c.PartsSent < len(parts) {
		nonce := PartNonce(rec.ChannelID, msgID, c.PartsSent)
		if _, err := m.API.MessageSendOnce(ctx, rec.ChannelID, &discordgo.MessageSend{Content: parts[c.PartsSent]}, nonce); err != nil {
			return fmt.Errorf("posting message part %d: %w", c.PartsSent+1, err)
		}
		c.PartsSent++
		if c.PartsSent < len(parts) {
			if err := m.save(ctx, *rec); err != nil {
				return fmt.Errorf("saving message progress: %w", err)
			}
		}
	}
	return nil
}

// PartNonce identifies one part of one Kandev message in one channel, in
// Discord's 25-character nonce limit.
func PartNonce(channelID, msgID string, part int) string {
	return discordapi.Nonce(channelID, msgID, strconv.Itoa(part))
}

func (m *Mirror) save(ctx context.Context, rec store.Channel) error {
	if m.Save == nil {
		return nil
	}
	return m.Save(ctx, rec)
}

// second parses a timestamp at the Host DTO's whole-second precision.
func second(s string) time.Time { return parseTime(s).Truncate(time.Second) }

// heldBack marks the newest agent message of each session that was running
// in the read before or after the messages, or changed state between them.
func heldBack(msgs []pluginsdk.Message, before, after []pluginsdk.Session) map[string]bool {
	running := map[string]bool{}
	state := map[string]string{}
	for _, s := range before {
		state[s.ID] = s.State
		if Running(s.State) {
			running[s.ID] = true
		}
	}
	for _, s := range after {
		if Running(s.State) || state[s.ID] != s.State {
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
// changes, at most every CardDebounce. A failed pin is retried on later
// syncs without reposting the card.
func (m *Mirror) card(ctx context.Context, rec *store.Channel, in CardInput, now time.Time) (time.Duration, error) {
	embed, hash := Card(in)
	var retry time.Duration
	if rec.CardMessageID != "" && rec.CardHash != hash {
		if wait := CardDebounce - now.Sub(rec.CardEditedAt); wait > 0 {
			retry = wait
		} else {
			embeds := []*discordgo.MessageEmbed{embed}
			_, err := m.API.MessageEdit(ctx, &discordgo.MessageEdit{Channel: rec.ChannelID, ID: rec.CardMessageID, Embeds: &embeds})
			switch {
			case err == nil:
				rec.CardHash, rec.CardEditedAt = hash, now
			case discordapi.HasCode(err, discordapi.CodeUnknownMessage):
				rec.CardMessageID, rec.CardPinned = "", false // deleted in Discord: post a new card
			default:
				return 0, fmt.Errorf("editing task card: %w", err)
			}
		}
	}
	if rec.CardMessageID == "" {
		// CardPosts makes a retry after a lost reply reuse the nonce, and a
		// replacement for a deleted card use a new one.
		nonce := discordapi.Nonce(rec.ChannelID, "card", strconv.Itoa(rec.CardPosts))
		msg, err := m.API.MessageSendOnce(ctx, rec.ChannelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}}, nonce)
		if err != nil {
			return 0, fmt.Errorf("posting task card: %w", err)
		}
		rec.CardMessageID, rec.CardHash, rec.CardEditedAt, rec.CardPinned = msg.ID, hash, now, false
		rec.CardPosts++
	}
	if !rec.CardPinned {
		err := m.API.MessagePin(ctx, rec.ChannelID, rec.CardMessageID)
		if err != nil && !discordapi.HasCode(err, discordapi.CodeMaxPinsReached) {
			return retry, fmt.Errorf("pinning task card: %w", err)
		}
		rec.CardPinned = true // pinned, or the channel's pin limit makes retrying pointless
	}
	return retry, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
