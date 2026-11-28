package outbound

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

func TestChunkSplitsOnLinesWithinLimit(t *testing.T) {
	text := strings.Repeat("hello world line\n", 300)
	chunks := Chunk(text, 2000)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), 2000)
		assert.True(t, strings.HasSuffix(c, "hello world line"), "split on a line boundary")
	}
	assert.Equal(t, strings.TrimSpace(text), strings.Join(chunks, "\n"))
}

func TestChunkKeepsCodeFencesBalanced(t *testing.T) {
	var b strings.Builder
	b.WriteString("Here is the fix:\n```go\n")
	for range 200 {
		b.WriteString("fmt.Println(\"x\")\n")
	}
	b.WriteString("```\nDone.")
	chunks := Chunk(b.String(), 2000)
	require.Greater(t, len(chunks), 1)
	for i, c := range chunks {
		assert.LessOrEqual(t, len(c), 2000)
		assert.Equal(t, 0, strings.Count(c, "```")%2, "chunk %d has balanced fences", i)
		if i > 0 && i < len(chunks)-1 {
			assert.True(t, strings.HasPrefix(c, "```go\n"), "chunk %d reopens the fence", i)
		}
	}
	assert.True(t, strings.HasSuffix(chunks[len(chunks)-1], "Done."))
}

func TestChunkHardSplitsLongLinesOnRuneBoundaries(t *testing.T) {
	chunks := Chunk(strings.Repeat("é", 1500), 2000)
	require.Len(t, chunks, 2)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), 2000)
		assert.True(t, strings.Count(c, "é")*2 == len(c), "no split rune")
	}
}

func TestChunksFitAndBalanceAroundFenceBoundaries(t *testing.T) {
	for n := 1980; n <= 2010; n++ {
		x := strings.Repeat("x", n)
		for _, text := range []string{"```" + x, "```" + x + "\nmore", "```go\n" + x + "\n```", "a\n```" + x, "```\n" + x,
			"```go\n" + x, "y\n```go\n" + x + "\n```\n" + x, "```go\n" + x + x + "\n```",
			"```go" + strings.Repeat(" ", n) + "\nbody\n```"} {
			for _, c := range Chunk(text, 2000) {
				require.LessOrEqual(t, len(c), 2000, "n=%d", n)
				delimiters := 0
				for _, line := range strings.Split(c, "\n") {
					if isFence(line) {
						delimiters++
					}
				}
				require.Zero(t, delimiters%2, "n=%d: every opened block is closed in its chunk: %q…", n, c[:min(len(c), 40)])
			}
		}
	}
}

func TestChunkTerminatesOnHugeFenceOpenersAndLines(t *testing.T) {
	text := "```" + strings.Repeat("x", 5000) + "\n" + strings.Repeat("code line\n", 1000) + "```"
	done := make(chan []string, 1)
	go func() { done <- Chunk(text, 2000) }()
	select {
	case chunks := <-done:
		require.NotEmpty(t, chunks)
		for _, c := range chunks {
			assert.LessOrEqual(t, len(c), 2000)
		}
		assert.LessOrEqual(t, len(chunks), MaxChunks+1, "stops once truncation is certain")
	case <-time.After(2 * time.Second):
		t.Fatal("Chunk did not terminate")
	}
	assert.LessOrEqual(t, len(Chunk(strings.Repeat("y", 10_000_000), 2000)), MaxChunks+1)
}

func TestLimitCapsAtFiveChunks(t *testing.T) {
	chunks, truncated := Limit(Chunk(strings.Repeat(strings.Repeat("x", 1999)+"\n", 7), 2000))
	assert.Len(t, chunks, MaxChunks)
	assert.True(t, truncated)
}

func TestRenderFilters(t *testing.T) {
	all := Options{MirrorUserMessages: true, PostToolActivity: true}
	cases := []struct {
		name string
		msg  pluginsdk.Message
		opts Options
		want string
	}{
		{"agent message", pluginsdk.Message{AuthorType: "agent", Type: "message", Content: " hi "}, Options{}, "hi"},
		{"agent content", pluginsdk.Message{AuthorType: "agent", Type: "content", Content: "x"}, Options{}, "x"},
		{"thinking never", pluginsdk.Message{AuthorType: "agent", Type: "thinking", Content: "hmm"}, all, ""},
		{"user mirrored", pluginsdk.Message{AuthorType: "user", Type: "message", Content: "do it"}, all, "**Kandev:** do it"},
		{"user off", pluginsdk.Message{AuthorType: "user", Type: "message", Content: "do it"}, Options{}, ""},
		{"own echo", pluginsdk.Message{AuthorType: "user", Type: "message", Content: "a (Discord): b", Source: PluginSource}, all, ""},
		{"other plugin", pluginsdk.Message{AuthorType: "user", Type: "message", Content: "c", Source: "plugin:other"}, all, "**Kandev:** c"},
		{"tool on", pluginsdk.Message{AuthorType: "agent", Type: "tool_execute", Content: "go test ./...\nok"}, all, "🔧 `go test ./...`"},
		{"tool off", pluginsdk.Message{AuthorType: "agent", Type: "tool_execute", Content: "go test"}, Options{}, ""},
		{"status never", pluginsdk.Message{AuthorType: "agent", Type: "status", Content: "x"}, all, ""},
	}
	for _, tc := range cases {
		got, ok := Render(tc.msg, tc.opts)
		assert.Equal(t, tc.want, got, tc.name)
		assert.Equal(t, tc.want != "", ok, tc.name)
	}
}

func TestStatusLines(t *testing.T) {
	s := func(state string) []pluginsdk.Session {
		return []pluginsdk.Session{{ID: "s1", State: state, AgentDisplayName: "Claude"}}
	}
	line := func(text string) []StatusLine { return []StatusLine{{SessionID: "s1", Text: text}} }
	lines, rec := StatusLines(nil, s("RUNNING"))
	assert.Equal(t, line("▶️ **Claude** is working."), lines)
	lines, rec = StatusLines(rec, s("RUNNING"))
	assert.Empty(t, lines)
	lines, rec = StatusLines(rec, s("WAITING_FOR_INPUT"))
	assert.Equal(t, line("✅ **Claude** finished its turn and is waiting for input."), lines)
	lines, _ = StatusLines(rec, s("FAILED"))
	assert.Equal(t, line("❌ **Claude** failed."), lines)
	lines, _ = StatusLines(nil, s("COMPLETED"))
	assert.Empty(t, lines, "first observation of a finished session is silent")
}

type mirrorFixture struct {
	api   *discordapi.Fake
	host  *kandev.Fake
	m     *Mirror
	rec   store.Channel
	task  *pluginsdk.Task
	saves int
}

func newMirror(t *testing.T) *mirrorFixture {
	t.Helper()
	f := &mirrorFixture{api: discordapi.NewFake(), host: kandev.NewFake()}
	ch := f.api.AddChannel(&discordgo.Channel{GuildID: "g", Name: "kan-1"})
	f.task = f.host.AddTask(pluginsdk.Task{ID: "t1", Identifier: "KAN-1", Title: "One", State: "IN_PROGRESS"})
	f.rec = store.Channel{TaskID: "t1", ChannelID: ch.ID, Status: store.StatusActive, OutboundCursor: store.Cursor{CreatedAt: "2026-11-25T10:00:00Z"}}
	f.m = &Mirror{
		API: f.api, Host: f.host, TaskURL: func(id string) string { return "https://k/t/" + id },
		Opts: Options{MirrorUserMessages: true},
		Save: func(context.Context, store.Channel) error { f.saves++; return nil },
	}
	return f
}

func (f *mirrorFixture) sync(t *testing.T, sessions []pluginsdk.Session, now time.Time) time.Duration {
	t.Helper()
	res, err := f.m.Sync(context.Background(), Input{
		Task: f.task, Rec: f.rec, Sessions: sessions, Now: now,
		Card: CardInput{Task: f.task, Sessions: sessions},
	})
	require.NoError(t, err)
	f.rec = res.Rec
	return res.Retry
}

// try syncs rec without sessions and returns the outcome, errors included.
func (f *mirrorFixture) try(rec store.Channel) (store.Channel, error) {
	res, err := f.m.Sync(context.Background(), Input{Task: f.task, Rec: rec, Now: time.Now(), Card: CardInput{Task: f.task}})
	return res.Rec, err
}

func (f *mirrorFixture) texts() []string {
	var out []string
	for _, m := range f.api.SentMessages(f.rec.ChannelID) {
		if m.Content != "" {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestTurnStartingDuringTheReadHoldsItsMessage(t *testing.T) {
	f := newMirror(t)
	now := time.Date(2026, 11, 25, 10, 5, 0, 0, time.UTC)
	waiting := []pluginsdk.Session{{ID: "s1", State: "WAITING_FOR_INPUT"}}
	f.host.Sessions["t1"] = []pluginsdk.Session{{ID: "s1", State: "RUNNING"}} // the turn started after the first read
	f.host.AddMessage(pluginsdk.Message{ID: "a", SessionID: "s1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "partial", CreatedAt: "2026-11-25T10:01:00Z"})
	res, err := f.m.Sync(context.Background(), Input{Task: f.task, Rec: f.rec, Sessions: waiting, Now: now, Card: CardInput{Task: f.task}})
	require.NoError(t, err)
	assert.True(t, res.Held)
	assert.Empty(t, f.texts())

	f.host.Messages["t1"][0].Content = "partial, now complete"
	f.host.Sessions["t1"] = waiting
	f.rec = res.Rec
	f.sync(t, waiting, now)
	assert.Equal(t, []string{"partial, now complete"}, f.texts())
}

func TestMirrorPostsInOrderOnceAndAdvancesCursor(t *testing.T) {
	f := newMirror(t)
	now := time.Date(2026, 11, 25, 10, 5, 0, 0, time.UTC)
	f.host.AddMessage(pluginsdk.Message{ID: "old", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "before cursor", CreatedAt: "2026-11-25T09:00:00Z"})
	f.host.AddMessage(pluginsdk.Message{ID: "b", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "second", CreatedAt: "2026-11-25T10:01:00Z"})
	f.host.AddMessage(pluginsdk.Message{ID: "a", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "first", CreatedAt: "2026-11-25T10:00:30Z"})
	f.host.AddMessage(pluginsdk.Message{ID: "c", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "same second", CreatedAt: "2026-11-25T10:01:00Z"})
	f.sync(t, nil, now)
	assert.Equal(t, []string{"first", "second", "same second"}, f.texts())
	cur := f.rec.OutboundCursor
	assert.Equal(t, "2026-11-25T10:01:00Z", cur.CreatedAt)
	assert.Equal(t, []string{"b", "c"}, cur.IDs)
	assert.Empty(t, cur.PartialID)
	assert.Equal(t, 3, f.saves, "saved after each posted message")

	f.api.ResetCalls()
	f.sync(t, nil, now)
	assert.Empty(t, f.api.CallsTo("MessageSend"), "duplicate trigger posts nothing")
}

func TestMirrorHoldsBackTheGrowingMessageOfARunningSession(t *testing.T) {
	f := newMirror(t)
	now := time.Date(2026, 11, 25, 10, 5, 0, 0, time.UTC)
	f.host.AddMessage(pluginsdk.Message{ID: "a", SessionID: "s1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "done part", CreatedAt: "2026-11-25T10:01:00Z"})
	f.host.AddMessage(pluginsdk.Message{ID: "b", SessionID: "s1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "still stream", CreatedAt: "2026-11-25T10:02:00Z"})
	running := []pluginsdk.Session{{ID: "s1", State: "RUNNING"}}
	f.sync(t, running, now)
	assert.Contains(t, f.texts(), "done part")
	assert.NotContains(t, f.texts(), "still stream")

	f.host.Messages["t1"][1].Content = "still streaming, now complete"
	f.sync(t, []pluginsdk.Session{{ID: "s1", State: "WAITING_FOR_INPUT"}}, now)
	assert.Contains(t, f.texts(), "still streaming, now complete")
}

func TestMirrorTruncatesLongMessagesWithALink(t *testing.T) {
	f := newMirror(t)
	f.host.AddMessage(pluginsdk.Message{ID: "a", TaskID: "t1", AuthorType: "agent", Type: "message", Content: strings.Repeat(strings.Repeat("y", 1990)+"\n", 8), CreatedAt: "2026-11-25T10:01:00Z"})
	f.sync(t, nil, time.Now())
	texts := f.texts()
	require.Len(t, texts, MaxChunks+1)
	assert.Equal(t, "… truncated — open in Kandev: https://k/t/t1", texts[MaxChunks])
}

func TestLostReplyIsNotRepostedOnRetry(t *testing.T) {
	f := newMirror(t)
	f.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "Done.", CreatedAt: "2026-11-25T10:01:00Z"})
	f.api.LoseNextReply("MessageSend")
	_, err := f.try(f.rec)
	require.Error(t, err)

	f.sync(t, nil, time.Now()) // the record is unchanged, as after a restart
	assert.Equal(t, []string{"Done."}, f.texts(), "the part's nonce makes Discord return the first post")
	assert.Equal(t, []string{"m1"}, f.rec.OutboundCursor.IDs)
}

func TestFailedPartIsResentEvenIfTheChannelShowsTheSameText(t *testing.T) {
	f := newMirror(t)
	_, err := f.api.MessageSend(context.Background(), f.rec.ChannelID, &discordgo.MessageSend{Content: "Done."})
	require.NoError(t, err)
	f.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "Done.", CreatedAt: "2026-11-25T10:01:00Z"})
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 500})
	_, err = f.try(f.rec)
	require.Error(t, err)

	f.sync(t, nil, time.Now())
	assert.Equal(t, []string{"Done.", "Done."}, f.texts(), "an earlier identical bot message is not mistaken for the part")
}

func TestPartialProgressDoesNotFollowTheTaskToANewChannel(t *testing.T) {
	f := newMirror(t)
	body := strings.Repeat("a", 1990) + "\n" + strings.Repeat("b", 1990)
	f.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: body, CreatedAt: "2026-11-25T10:01:00Z"})
	f.api.FailNext("MessageSend", nil)
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 500})
	rec, err := f.try(f.rec)
	require.Error(t, err)
	require.Equal(t, 1, rec.OutboundCursor.PartsSent)

	replacement := f.api.AddChannel(&discordgo.Channel{GuildID: "g", Name: "kan-1"})
	rec.ChannelID = replacement.ID
	f.rec = rec
	f.sync(t, nil, time.Now())
	texts := f.texts()
	require.Len(t, texts, 2, "the new channel gets the whole message")
	assert.True(t, strings.HasPrefix(texts[0], "aaa"))
	assert.True(t, strings.HasPrefix(texts[1], "bbb"))
}

func TestFractionalCursorKeepsRowsOfItsFirstSecond(t *testing.T) {
	f := newMirror(t)
	f.rec.OutboundCursor = store.Cursor{CreatedAt: "2026-11-25T10:00:00.5Z"}
	// Created at 10:00:00.75, serialized by the Host in whole seconds.
	f.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: "early", CreatedAt: "2026-11-25T10:00:00Z"})
	f.sync(t, nil, time.Now())
	assert.Equal(t, []string{"early"}, f.texts())
	f.api.ResetCalls()
	f.sync(t, nil, time.Now())
	assert.Empty(t, f.api.CallsTo("MessageSend"))
}

func TestFailedPartResumesWithoutRepeatingDeliveredParts(t *testing.T) {
	f := newMirror(t)
	var saved store.Channel
	f.m.Save = func(_ context.Context, r store.Channel) error { saved = r; return nil }
	body := strings.Repeat("a", 1990) + "\n" + strings.Repeat("b", 1990) + "\n" + strings.Repeat("c", 10)
	f.host.AddMessage(pluginsdk.Message{ID: "m1", TaskID: "t1", AuthorType: "agent", Type: "message", Content: body, CreatedAt: "2026-11-25T10:01:00Z"})
	f.api.FailNext("MessageSend", nil)
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 500})
	_, err := f.try(f.rec)
	require.Error(t, err)
	assert.Equal(t, 1, saved.OutboundCursor.PartsSent)
	assert.Equal(t, "m1", saved.OutboundCursor.PartialID)

	f.rec = saved
	f.sync(t, nil, time.Now())
	texts := f.texts()
	require.Len(t, texts, 3, "each part posted exactly once")
	assert.True(t, strings.HasPrefix(texts[0], "aaa"))
	assert.True(t, strings.HasPrefix(texts[1], "bbb"))
	assert.Equal(t, strings.Repeat("c", 10), texts[2])
	assert.Empty(t, f.rec.OutboundCursor.PartialID)
}

func TestFailedPinIsRetriedWithoutRepostingTheCard(t *testing.T) {
	f := newMirror(t)
	f.api.FailNext("MessagePin", &discordapi.Error{Status: 500})
	rec, err := f.try(f.rec)
	require.Error(t, err)
	require.NotEmpty(t, rec.CardMessageID)
	assert.False(t, rec.CardPinned)
	card := rec.CardMessageID
	rec, err = f.try(rec)
	require.NoError(t, err)
	assert.Equal(t, card, rec.CardMessageID, "card not reposted")
	assert.True(t, rec.CardPinned)
	assert.True(t, f.api.Pins[card])
}

func TestCardIsPinnedOnceAndEditsAreDebounced(t *testing.T) {
	f := newMirror(t)
	now := time.Date(2026, 11, 25, 10, 5, 0, 0, time.UTC)
	f.sync(t, nil, now)
	require.NotEmpty(t, f.rec.CardMessageID)
	assert.True(t, f.api.Pins[f.rec.CardMessageID])

	f.api.ResetCalls()
	f.sync(t, nil, now.Add(time.Second))
	assert.Equal(t, 0, f.api.Mutations(), "unchanged card is not edited")

	f.task.State = "REVIEW"
	retry := f.sync(t, nil, now.Add(2*time.Second))
	assert.Equal(t, 8*time.Second, retry)
	assert.Empty(t, f.api.CallsTo("MessageEdit"))
	f.sync(t, nil, now.Add(11*time.Second))
	assert.Len(t, f.api.CallsTo("MessageEdit"), 1)
	card := f.api.Message(f.rec.ChannelID, f.rec.CardMessageID)
	assert.Equal(t, "review", card.Embeds[0].Fields[0].Value)
}

func TestCardReturnedByARetriedPostIsEditedToCurrentContents(t *testing.T) {
	f := newMirror(t)
	f.api.LoseNextReply("MessageSend")
	_, err := f.try(f.rec)
	require.Error(t, err)
	f.task.Title = "Renamed" // the task changed before the retry
	now := time.Now()
	f.sync(t, nil, now) // Discord returns the first card (same nonce)
	f.sync(t, nil, now.Add(time.Minute))
	sent := f.api.SentMessages(f.rec.ChannelID)
	require.Len(t, sent, 1, "one card")
	assert.Contains(t, sent[0].Embeds[0].Title, "Renamed")
}

func TestStatusLineRetriedAfterALostReplyIsPostedOnce(t *testing.T) {
	f := newMirror(t)
	sessions := []pluginsdk.Session{{ID: "s1", State: "RUNNING", AgentDisplayName: "Claude"}}
	f.host.Sessions["t1"] = sessions
	f.api.LoseNextReply("MessageSend")
	res, err := f.m.Sync(context.Background(), Input{Task: f.task, Rec: f.rec, Sessions: sessions, Now: time.Now(), Card: CardInput{Task: f.task}})
	require.Error(t, err)
	f.rec = res.Rec
	f.sync(t, sessions, time.Now())
	assert.Equal(t, []string{"▶️ **Claude** is working."}, f.texts())

	// A later identical line (a new turn) is still posted.
	f.host.Sessions["t1"][0].State = "WAITING_FOR_INPUT"
	f.sync(t, f.host.Sessions["t1"], time.Now())
	f.host.Sessions["t1"][0].State = "RUNNING"
	f.sync(t, f.host.Sessions["t1"], time.Now())
	assert.Len(t, f.texts(), 3)
}

func TestPartlyPostedStatusBatchIsNotRepostedOnRetry(t *testing.T) {
	f := newMirror(t)
	sessions := []pluginsdk.Session{
		{ID: "s1", State: "RUNNING", AgentDisplayName: "Claude", StartedAt: "2026-11-25T10:00:00Z"},
		{ID: "s2", State: "RUNNING", AgentDisplayName: "Codex", StartedAt: "2026-11-25T10:00:01Z"},
	}
	f.host.Sessions["t1"] = sessions
	f.api.FailNext("MessageSend", nil)                            // line 1 posts
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 503}) // line 2 fails
	res, err := f.m.Sync(context.Background(), Input{Task: f.task, Rec: f.rec, Sessions: sessions, Now: time.Now(), Card: CardInput{Task: f.task}})
	require.Error(t, err)
	f.rec = res.Rec // saved, as syncActive does
	f.sync(t, sessions, time.Now())
	assert.Equal(t, []string{"▶️ **Claude** is working.", "▶️ **Codex** is working."}, f.texts())
}

func TestStatusRetryWithAnEarlierSessionJoiningIsNotReposted(t *testing.T) {
	f := newMirror(t)
	f.rec.SessionStates = map[string]string{"s0": "IDLE", "s1": "IDLE", "s2": "IDLE"}
	sessions := []pluginsdk.Session{
		{ID: "s0", State: "IDLE", AgentDisplayName: "A", StartedAt: "2026-11-25T09:00:00Z"},
		{ID: "s1", State: "RUNNING", AgentDisplayName: "B", StartedAt: "2026-11-25T10:00:00Z"},
		{ID: "s2", State: "RUNNING", AgentDisplayName: "C", StartedAt: "2026-11-25T10:00:01Z"},
	}
	f.host.Sessions["t1"] = sessions
	f.api.FailNext("MessageSend", nil)                            // B posts
	f.api.FailNext("MessageSend", &discordapi.Error{Status: 503}) // C fails
	res, err := f.m.Sync(context.Background(), Input{Task: f.task, Rec: f.rec, Sessions: sessions, Now: time.Now(), Card: CardInput{Task: f.task}})
	require.Error(t, err)
	f.rec = res.Rec
	later := slices.Clone(sessions)
	later[0].State = "RUNNING" // A, which sorts first, starts before the retry
	f.host.Sessions["t1"] = later
	f.sync(t, later, time.Now())
	assert.Equal(t, []string{"▶️ **B** is working.", "▶️ **A** is working.", "▶️ **C** is working."}, f.texts())
}
