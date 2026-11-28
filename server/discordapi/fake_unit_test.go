package discordapi

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Fake stands in for Discord in every package's tests, so its Discord
// semantics (nonces, lost replies, 404s, ordering) are part of its contract.

func TestFakeSendOnceDeduplicatesByChannelAndNonce(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	ch := f.AddChannel(&discordgo.Channel{GuildID: "g"})
	other := f.AddChannel(&discordgo.Channel{GuildID: "g"})

	first, err := f.MessageSendOnce(ctx, ch.ID, &discordgo.MessageSend{Content: "a"}, "n1")
	require.NoError(t, err)
	again, err := f.MessageSendOnce(ctx, ch.ID, &discordgo.MessageSend{Content: "a"}, "n1")
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID)
	elsewhere, err := f.MessageSendOnce(ctx, other.ID, &discordgo.MessageSend{Content: "a"}, "n1")
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, elsewhere.ID, "nonces are scoped to a channel")

	assert.Len(t, f.SentMessages(ch.ID), 1)
	assert.Len(t, f.CallsTo("MessageSend"), 3, "SendOnce is recorded as MessageSend")
}

func TestFakeLostReplyStoresTheMessage(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	ch := f.AddChannel(&discordgo.Channel{GuildID: "g"})
	f.LoseNextReply("MessageSend")

	_, err := f.MessageSendOnce(ctx, ch.ID, &discordgo.MessageSend{Content: "a"}, "n1")
	require.Error(t, err)
	assert.Equal(t, 502, Status(err))
	sent := f.SentMessages(ch.ID)
	require.Len(t, sent, 1, "Discord stored the post")

	retry, err := f.MessageSendOnce(ctx, ch.ID, &discordgo.MessageSend{Content: "a"}, "n1")
	require.NoError(t, err)
	assert.Equal(t, sent[0].ID, retry.ID, "the nonce retry returns the stored post")
}

func TestFakeFailNextQueuesErrorsInOrder(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	e1, e2 := errors.New("one"), errors.New("two")
	f.FailNext("GuildRoles", e1)
	f.FailNext("GuildRoles", e2)
	_, err := f.GuildRoles(ctx, "g")
	assert.ErrorIs(t, err, e1)
	_, err = f.GuildRoles(ctx, "g")
	assert.ErrorIs(t, err, e2)
	_, err = f.GuildRoles(ctx, "g")
	assert.NoError(t, err)
	assert.Zero(t, f.Mutations(), "reads are not mutations")

	f.ResetCalls()
	assert.Empty(t, f.Calls())
}

func TestFakeChannelLifecycle(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	cat, err := f.GuildChannelCreate(ctx, "g", discordgo.GuildChannelCreateData{Name: "kandev", Type: discordgo.ChannelTypeGuildCategory})
	require.NoError(t, err)
	ch, err := f.GuildChannelCreate(ctx, "g", discordgo.GuildChannelCreateData{Name: "task", ParentID: cat.ID, Topic: "t"})
	require.NoError(t, err)
	f.AddChannel(&discordgo.Channel{GuildID: "other"})

	list, err := f.GuildChannels(ctx, "g")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{cat.ID, ch.ID}, []string{list[0].ID, list[1].ID}, "ordered by ID")
	list[0].Name = "mutated"
	assert.Equal(t, "kandev", f.GuildChannelList("g")[0].Name, "callers get copies")

	edited, err := f.ChannelEdit(ctx, ch.ID, &discordgo.ChannelEdit{Name: "renamed", Topic: "new", ParentID: "p",
		PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: "r"}}})
	require.NoError(t, err)
	assert.Equal(t, "renamed", edited.Name)
	assert.Equal(t, "new", edited.Topic)
	assert.Equal(t, "p", edited.ParentID)
	assert.Len(t, edited.PermissionOverwrites, 1)
	kept, err := f.ChannelEdit(ctx, ch.ID, &discordgo.ChannelEdit{})
	require.NoError(t, err)
	assert.Equal(t, "renamed", kept.Name, "empty fields leave values unchanged")

	f.AddMessage(&discordgo.Message{ChannelID: ch.ID})
	require.NoError(t, f.ChannelDelete(ctx, ch.ID))
	assert.Empty(t, f.Messages[ch.ID])
	assert.True(t, IsGone(f.ChannelDelete(ctx, ch.ID)))
	_, err = f.ChannelEdit(ctx, ch.ID, &discordgo.ChannelEdit{Name: "x"})
	assert.True(t, HasCode(err, CodeUnknownChannel))
	_, err = f.MessageSend(ctx, ch.ID, &discordgo.MessageSend{Content: "x"})
	assert.True(t, IsGone(err))
	assert.Equal(t, 8, f.Mutations(), "failed mutations are still recorded")
}

func TestFakeMessagesEditReactPin(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	ch := f.AddChannel(&discordgo.Channel{GuildID: "g"})
	m, err := f.MessageSend(ctx, ch.ID, &discordgo.MessageSend{Content: "a"})
	require.NoError(t, err)

	content := "b"
	embeds := []*discordgo.MessageEmbed{{Title: "e"}}
	comps := []discordgo.MessageComponent{discordgo.ActionsRow{}}
	_, err = f.MessageEdit(ctx, &discordgo.MessageEdit{Channel: ch.ID, ID: m.ID, Content: &content, Embeds: &embeds, Components: &comps})
	require.NoError(t, err)
	got := f.Message(ch.ID, m.ID)
	require.NotNil(t, got)
	assert.Equal(t, "b", got.Content)
	assert.Len(t, got.Embeds, 1)
	assert.Len(t, got.Components, 1)
	assert.Nil(t, f.Message(ch.ID, "missing"))
	_, err = f.MessageEdit(ctx, &discordgo.MessageEdit{Channel: ch.ID, ID: "missing", Content: &content})
	assert.True(t, HasCode(err, CodeUnknownMessage))

	require.NoError(t, f.ReactionAdd(ctx, ch.ID, m.ID, "✅"))
	assert.Equal(t, []string{"✅"}, f.ReactionsOn(m.ID))
	require.NoError(t, f.MessagePin(ctx, ch.ID, m.ID))
	assert.True(t, f.Pins[m.ID])

	incoming := f.AddMessage(&discordgo.Message{ChannelID: ch.ID, Author: &discordgo.User{ID: "human"}})
	assert.Len(t, f.SentMessages(ch.ID), 1, "only the bot's messages")
	assert.NotEqual(t, incoming.ID, m.ID)
}

func TestFakeChannelMessagesHonoursLimit(t *testing.T) {
	f := NewFake()
	ch := f.AddChannel(&discordgo.Channel{GuildID: "g"})
	var ids []string
	for range 4 {
		ids = append(ids, f.AddMessage(&discordgo.Message{ChannelID: ch.ID}).ID)
	}
	got, err := f.ChannelMessages(context.Background(), ch.ID, 2, "")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{ids[1], ids[0]}, []string{got[0].ID, got[1].ID}, "oldest page, newest first")
}

func TestFakeIdentityAndGuildReads(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	f.Application = &discordgo.Application{ID: "app"}
	f.AddGuild("g", "Guild", 8)
	f.Members["u"] = &discordgo.Member{Roles: []string{"r"}}

	u, err := f.CurrentUser(ctx)
	require.NoError(t, err)
	assert.True(t, u.Bot)
	app, err := f.CurrentApplication(ctx)
	require.NoError(t, err)
	assert.Equal(t, "app", app.ID)
	gs, err := f.CurrentUserGuilds(ctx)
	require.NoError(t, err)
	require.Len(t, gs, 1)
	assert.Equal(t, int64(8), gs[0].Permissions)
	m, err := f.GuildMember(ctx, "g", "u")
	require.NoError(t, err)
	assert.Equal(t, []string{"r"}, m.Roles)
	_, err = f.GuildMember(ctx, "g", "stranger")
	assert.True(t, IsGone(err))

	role, err := f.GuildRoleCreate(ctx, "g", &discordgo.RoleParams{Name: "kandev"})
	require.NoError(t, err)
	roles, _ := f.GuildRoles(ctx, "g")
	assert.Equal(t, role.ID, roles[0].ID)
	require.NoError(t, f.BulkOverwriteGuildCommands(ctx, "app", "g", []*discordgo.ApplicationCommand{{Name: "kandev"}}))
	assert.Len(t, f.Commands["g"], 1)
	assert.Equal(t, "BulkOverwriteGuildCommands[app g 1]", f.CallsTo("BulkOverwriteGuildCommands")[0].String())

	sentinel := errors.New("down")
	for _, method := range []string{"CurrentUser", "CurrentApplication", "CurrentUserGuilds", "GuildMember", "GuildRoleCreate",
		"GuildChannels", "GuildChannelCreate", "ChannelEdit", "ChannelDelete", "ChannelMessages", "MessageSend", "MessageEdit",
		"ReactionAdd", "MessagePin", "BulkOverwriteGuildCommands", "InteractionRespond", "InteractionEdit"} {
		f.FailNext(method, sentinel)
	}
	_, e1 := f.CurrentUser(ctx)
	_, e2 := f.CurrentApplication(ctx)
	_, e3 := f.CurrentUserGuilds(ctx)
	_, e4 := f.GuildMember(ctx, "g", "u")
	_, e5 := f.GuildRoleCreate(ctx, "g", &discordgo.RoleParams{})
	_, e6 := f.GuildChannels(ctx, "g")
	_, e7 := f.GuildChannelCreate(ctx, "g", discordgo.GuildChannelCreateData{})
	_, e8 := f.ChannelEdit(ctx, "c", &discordgo.ChannelEdit{})
	e9 := f.ChannelDelete(ctx, "c")
	_, e10 := f.ChannelMessages(ctx, "c", 1, "")
	_, e11 := f.MessageSendOnce(ctx, "c", &discordgo.MessageSend{}, "n")
	_, e12 := f.MessageEdit(ctx, &discordgo.MessageEdit{})
	e13 := f.ReactionAdd(ctx, "c", "m", "x")
	e14 := f.MessagePin(ctx, "c", "m")
	e15 := f.BulkOverwriteGuildCommands(ctx, "a", "g", nil)
	e16 := f.InteractionRespond(ctx, &discordgo.Interaction{ID: "i"}, &discordgo.InteractionResponse{})
	e17 := f.InteractionEdit(ctx, &discordgo.Interaction{ID: "i"}, &discordgo.WebhookEdit{})
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7, e8, e9, e10, e11, e12, e13, e14, e15, e16, e17} {
		assert.ErrorIs(t, err, sentinel, "call %d", i+1)
	}
	_, err = f.MessageSend(ctx, "c", &discordgo.MessageSend{})
	assert.True(t, IsGone(err), "the queued MessageSend error was consumed by SendOnce")
}

func TestFakeLastReplyPrefersLatestContent(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	i := &discordgo.Interaction{ID: "i"}
	assert.Empty(t, f.LastReply("i"))
	require.NoError(t, f.InteractionRespond(ctx, i, &discordgo.InteractionResponse{Data: &discordgo.InteractionResponseData{Content: "first"}}))
	assert.Equal(t, "first", f.LastReply("i"))
	require.NoError(t, f.InteractionEdit(ctx, i, &discordgo.WebhookEdit{}))
	assert.Equal(t, "first", f.LastReply("i"), "an edit without content keeps the earlier text")
	edited := "edited"
	require.NoError(t, f.InteractionEdit(ctx, i, &discordgo.WebhookEdit{Content: &edited}))
	assert.Equal(t, "edited", f.LastReply("i"))
}

func TestFakeIDsAreIncreasingSnowflakes(t *testing.T) {
	f := NewFake()
	a, b := f.NewID(), f.NewID()
	assert.Less(t, CompareIDs(a, b), 0)
	assert.Zero(t, CompareIDs(a, a))
	assert.Greater(t, CompareIDs(b, a), 0)

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f.StartIDsAt(at)
	id, err := strconv.ParseUint(f.NewID(), 10, 64)
	require.NoError(t, err)
	assert.Equal(t, at.UnixMilli(), int64(id>>22)+discordEpochMs, "IDs carry their creation time")
}
