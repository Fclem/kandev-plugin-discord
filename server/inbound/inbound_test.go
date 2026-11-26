package inbound

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// idAt builds a snowflake created at t.
func idAt(t time.Time, seq int) string {
	ms := uint64(t.UnixMilli() - 1420070400000)
	return strconv.FormatUint(ms<<22|uint64(seq), 10)
}

type fixture struct {
	api     *discordapi.Fake
	host    *kandev.Fake
	h       *Handler
	rec     store.Channel
	saved   []store.Channel
	ops     map[string]bool
	authErr error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{api: discordapi.NewFake(), host: kandev.NewFake(), ops: map[string]bool{"op": true}}
	ch := f.api.AddChannel(&discordgo.Channel{ID: idAt(now.Add(-time.Hour), 0), GuildID: "g"})
	f.rec = store.Channel{TaskID: "t1", ChannelID: ch.ID, Status: store.StatusActive, InboundCursor: ch.ID}
	f.h = &Handler{
		API: f.api, Host: f.host, MaxAge: 15 * time.Minute, Now: func() time.Time { return now },
		Save: func(_ context.Context, r store.Channel) error { f.saved = append(f.saved, r); return nil },
		Authorize: func(_ context.Context, m *discordgo.Message) (*discordgo.Member, bool, error) {
			if f.authErr != nil {
				return nil, false, f.authErr
			}
			return m.Member, f.ops[m.Author.ID], nil
		},
	}
	return f
}

func (f *fixture) msg(at time.Time, seq int, author, content string) *discordgo.Message {
	return &discordgo.Message{
		ID: idAt(at, seq), ChannelID: f.rec.ChannelID, GuildID: "g", Content: content,
		Author: &discordgo.User{ID: author, Username: author},
		Member: &discordgo.Member{Nick: "Alice"},
	}
}

func (f *fixture) handle(t *testing.T, m *discordgo.Message) {
	t.Helper()
	rec, err := f.h.Handle(context.Background(), f.rec, m)
	require.NoError(t, err)
	f.rec = rec
}

func TestOperatorMessageIsDeliveredWithReactionPerOutcome(t *testing.T) {
	f := newFixture(t)
	m := f.msg(now, 1, "op", "  please add tests ")
	f.handle(t, m)
	require.Len(t, f.host.Sent, 1)
	assert.Equal(t, kandev.SentMessage{TaskID: "t1", Text: "Alice (Discord): please add tests"}, f.host.Sent[0])
	assert.Equal(t, []string{ReactDelivered}, f.api.ReactionsOn(m.ID))
	assert.Equal(t, m.ID, f.rec.InboundCursor)
	assert.Empty(t, f.rec.InboundInflight)
	assert.Equal(t, m.ID, f.saved[0].InboundInflight, "in-flight marker saved before Send")

	f.host.SendFn = func(string, string) (*pluginsdk.MessageDispatch, error) {
		return &pluginsdk.MessageDispatch{Status: "queued"}, nil
	}
	q := f.msg(now, 2, "op", "next")
	f.handle(t, q)
	assert.Equal(t, []string{ReactQueued}, f.api.ReactionsOn(q.ID))

	f.host.SendFn = func(string, string) (*pluginsdk.MessageDispatch, error) {
		return nil, status.Error(codes.ResourceExhausted, "task session message queue is full")
	}
	full := f.msg(now, 3, "op", "one more")
	f.handle(t, full)
	assert.Equal(t, []string{ReactFailed}, f.api.ReactionsOn(full.ID))
	replies := f.api.SentMessages(f.rec.ChannelID)
	require.Len(t, replies, 1)
	assert.Contains(t, replies[0].Content, "queue is full")
	assert.Equal(t, full.ID, replies[0].MessageReference.MessageID)
	assert.Equal(t, full.ID, f.rec.InboundCursor, "final outcome advances the cursor")
}

func TestNonOperatorGetsOneRefusalAndNothingReachesKandev(t *testing.T) {
	f := newFixture(t)
	m := f.msg(now, 1, "guest", "hi")
	f.handle(t, m)
	f.handle(t, m)
	assert.Empty(t, f.host.Sent)
	assert.Equal(t, []string{ReactDenied}, f.api.ReactionsOn(m.ID))
}

func TestAlreadyHandledOrInFlightMessagesAreNeverSentTwice(t *testing.T) {
	f := newFixture(t)
	m := f.msg(now, 1, "op", "hi")
	f.handle(t, m)
	f.handle(t, m)
	assert.Len(t, f.host.Sent, 1)

	crashed := f.msg(now, 2, "op", "lost?")
	f.rec.InboundInflight = crashed.ID
	f.handle(t, crashed)
	assert.Len(t, f.host.Sent, 1, "in-flight message is not resent")
	assert.Equal(t, []string{ReactFailed}, f.api.ReactionsOn(crashed.ID))
}

func TestCatchUpDeliversRecentAndRejectsStaleMessages(t *testing.T) {
	f := newFixture(t)
	stale := f.api.AddMessage(f.msg(now.Add(-40*time.Minute), 1, "op", "old news"))
	bot := f.api.AddMessage(&discordgo.Message{ID: idAt(now.Add(-20*time.Minute), 1), ChannelID: f.rec.ChannelID, Content: "card", Author: f.api.BotUser})
	recent := f.api.AddMessage(f.msg(now.Add(-5*time.Minute), 1, "op", "still relevant"))

	rec, err := f.h.CatchUp(context.Background(), f.rec, "")
	require.NoError(t, err)
	assert.Equal(t, recent.ID, rec.InboundCursor)
	assert.Equal(t, []kandev.SentMessage{{TaskID: "t1", Text: "Alice (Discord): still relevant"}}, f.host.Sent)
	assert.Equal(t, []string{ReactFailed}, f.api.ReactionsOn(stale.ID))
	assert.Empty(t, f.api.ReactionsOn(bot.ID))
	assert.Equal(t, []string{ReactDelivered}, f.api.ReactionsOn(recent.ID))

	f.rec = rec
	again, err := f.h.CatchUp(context.Background(), f.rec, "")
	require.NoError(t, err)
	assert.Equal(t, rec.InboundCursor, again.InboundCursor)
	assert.Len(t, f.host.Sent, 1)
}

func TestUnknownAuthorityKeepsTheMessageForALaterCatchUp(t *testing.T) {
	f := newFixture(t)
	m := f.api.AddMessage(f.msg(now.Add(-time.Minute), 1, "op", "deploy"))
	f.authErr = errors.New("discord unavailable")
	rec, err := f.h.CatchUp(context.Background(), f.rec, "")
	require.Error(t, err)
	assert.Equal(t, f.rec.InboundCursor, rec.InboundCursor, "cursor does not pass an undecided message")
	assert.Empty(t, f.host.Sent)
	assert.Empty(t, f.api.ReactionsOn(m.ID))

	f.authErr = nil
	rec, err = f.h.CatchUp(context.Background(), rec, "")
	require.NoError(t, err)
	assert.Equal(t, m.ID, rec.InboundCursor)
	assert.Len(t, f.host.Sent, 1)
}

func TestDeliverableFiltersBotsWebhooksSystemAndEmpty(t *testing.T) {
	human := &discordgo.User{ID: "u"}
	assert.True(t, Deliverable(&discordgo.Message{Author: human, Content: "x"}))
	assert.False(t, Deliverable(&discordgo.Message{Author: &discordgo.User{Bot: true}, Content: "x"}))
	assert.False(t, Deliverable(&discordgo.Message{Author: human, WebhookID: "w", Content: "x"}))
	assert.False(t, Deliverable(&discordgo.Message{Author: human, Content: "   "}))
	assert.False(t, Deliverable(&discordgo.Message{Author: human, Type: discordgo.MessageTypeChannelPinnedMessage, Content: "x"}))
}
