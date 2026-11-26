package discordapi

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Call is one recorded Fake call.
type Call struct {
	Method string
	Args   []any
}

// readMethods never mutate Discord state.
var readMethods = map[string]bool{
	"CurrentUser": true, "CurrentApplication": true, "CurrentUserGuilds": true,
	"GuildRoles": true, "GuildChannels": true, "ChannelMessages": true, "GuildMember": true,
}

// Fake is an in-memory API for tests. It keeps roles, channels, messages,
// reactions, pins, and commands, records every call, and returns injected
// errors. IDs are increasing snowflake-shaped strings.
type Fake struct {
	mu sync.Mutex

	BotUser     *discordgo.User
	Application *discordgo.Application
	UserGuilds  []*discordgo.UserGuild

	Roles     map[string][]*discordgo.Role    // guild → roles
	Members   map[string]*discordgo.Member    // user id → member
	Channels  map[string]*discordgo.Channel   // channel id → channel
	Messages  map[string][]*discordgo.Message // channel id → messages, oldest first
	Reactions map[string][]string             // message id → emoji
	Pins      map[string]bool                 // message id
	Commands  map[string][]*discordgo.ApplicationCommand
	// Interactions records responses and edits per interaction ID.
	Interactions map[string][]any

	calls     []Call
	errs      map[string][]error
	lostReply map[string]int
	nonces    map[string]*discordgo.Message // channel + nonce → first message
	nextID    uint64
}

var _ API = (*Fake)(nil)

// NewFake returns an empty Fake with a bot user.
func NewFake() *Fake {
	return &Fake{
		BotUser:      &discordgo.User{ID: "900000000000000001", Username: "kandev", Bot: true},
		Roles:        map[string][]*discordgo.Role{},
		Members:      map[string]*discordgo.Member{},
		Channels:     map[string]*discordgo.Channel{},
		Messages:     map[string][]*discordgo.Message{},
		Reactions:    map[string][]string{},
		Pins:         map[string]bool{},
		Commands:     map[string][]*discordgo.ApplicationCommand{},
		Interactions: map[string][]any{},
		errs:         map[string][]error{},
		lostReply:    map[string]int{},
		nonces:       map[string]*discordgo.Message{},
		nextID:       1_000_000_000_000_000_000,
	}
}

// NewID returns the next snowflake-shaped ID.
func (f *Fake) NewID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newIDLocked()
}

func (f *Fake) newIDLocked() string {
	f.nextID++
	return strconv.FormatUint(f.nextID, 10)
}

// StartIDsAt makes later IDs carry creation time t, like Discord's.
func (f *Fake) StartIDsAt(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID = uint64(t.UnixMilli()-discordEpochMs) << 22
}

const discordEpochMs = 1420070400000

// LoseNextReply makes the next call of method take effect in Discord but
// return an error, like a response lost to a timeout.
func (f *Fake) LoseNextReply(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lostReply[method]++
}

func (f *Fake) lost(method string) error {
	if f.lostReply[method] == 0 {
		return nil
	}
	f.lostReply[method]--
	return &Error{Status: 502, Message: "response lost"}
}

// FailNext makes the next call of method return err (queued in order).
func (f *Fake) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = append(f.errs[method], err)
}

// Calls returns a copy of the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsTo returns the recorded calls of one method.
func (f *Fake) CallsTo(method string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Mutations counts recorded calls that change Discord state.
func (f *Fake) Mutations() int {
	n := 0
	for _, c := range f.Calls() {
		if !readMethods[c.Method] {
			n++
		}
	}
	return n
}

// ResetCalls forgets recorded calls, keeping state.
func (f *Fake) ResetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// AddGuild registers guild membership for the bot with permissions.
func (f *Fake) AddGuild(guildID, name string, permissions int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UserGuilds = append(f.UserGuilds, &discordgo.UserGuild{ID: guildID, Name: name, Permissions: permissions})
}

// AddChannel stores a channel directly (no call recorded).
func (f *Fake) AddChannel(ch *discordgo.Channel) *discordgo.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch.ID == "" {
		ch.ID = f.newIDLocked()
	}
	f.Channels[ch.ID] = ch
	return ch
}

// AddMessage stores an incoming message directly (no call recorded).
func (f *Fake) AddMessage(m *discordgo.Message) *discordgo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.ID == "" {
		m.ID = f.newIDLocked()
	}
	f.Messages[m.ChannelID] = append(f.Messages[m.ChannelID], m)
	return m
}

// Message returns a stored message.
func (f *Fake) Message(channelID, messageID string) *discordgo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.Messages[channelID] {
		if m.ID == messageID {
			return m
		}
	}
	return nil
}

// SentMessages returns the bot's messages in a channel, oldest first.
func (f *Fake) SentMessages(channelID string) []*discordgo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*discordgo.Message
	for _, m := range f.Messages[channelID] {
		if m.Author != nil && m.Author.ID == f.BotUser.ID {
			out = append(out, m)
		}
	}
	return out
}

// GuildChannelList returns the channels of a guild.
func (f *Fake) GuildChannelList(guildID string) []*discordgo.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.guildChannelsLocked(guildID)
}

func (f *Fake) guildChannelsLocked(guildID string) []*discordgo.Channel {
	var out []*discordgo.Channel
	for _, ch := range f.Channels {
		if ch.GuildID == guildID {
			out = append(out, ch)
		}
	}
	slices.SortFunc(out, func(a, b *discordgo.Channel) int { return compareIDs(a.ID, b.ID) })
	return out
}

// record logs a call and pops an injected error.
func (f *Fake) record(method string, args ...any) error {
	f.calls = append(f.calls, Call{Method: method, Args: args})
	if q := f.errs[method]; len(q) > 0 {
		f.errs[method] = q[1:]
		return q[0]
	}
	return nil
}

func (f *Fake) CurrentUser(context.Context) (*discordgo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CurrentUser"); err != nil {
		return nil, err
	}
	return f.BotUser, nil
}

func (f *Fake) CurrentApplication(context.Context) (*discordgo.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CurrentApplication"); err != nil {
		return nil, err
	}
	return f.Application, nil
}

func (f *Fake) CurrentUserGuilds(context.Context) ([]*discordgo.UserGuild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CurrentUserGuilds"); err != nil {
		return nil, err
	}
	return slices.Clone(f.UserGuilds), nil
}

func (f *Fake) GuildMember(_ context.Context, guildID, userID string) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("GuildMember", guildID, userID); err != nil {
		return nil, err
	}
	m, ok := f.Members[userID]
	if !ok {
		return nil, &Error{Status: 404, Code: 10007, Message: "Unknown Member"}
	}
	return m, nil
}

func (f *Fake) GuildRoles(_ context.Context, guildID string) ([]*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("GuildRoles", guildID); err != nil {
		return nil, err
	}
	return slices.Clone(f.Roles[guildID]), nil
}

func (f *Fake) GuildRoleCreate(_ context.Context, guildID string, params *discordgo.RoleParams) (*discordgo.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("GuildRoleCreate", guildID, params.Name); err != nil {
		return nil, err
	}
	r := &discordgo.Role{ID: f.newIDLocked(), Name: params.Name}
	f.Roles[guildID] = append(f.Roles[guildID], r)
	return r, nil
}

func (f *Fake) GuildChannels(_ context.Context, guildID string) ([]*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("GuildChannels", guildID); err != nil {
		return nil, err
	}
	var out []*discordgo.Channel
	for _, ch := range f.guildChannelsLocked(guildID) {
		c := *ch
		out = append(out, &c)
	}
	return out, nil
}

func (f *Fake) GuildChannelCreate(_ context.Context, guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("GuildChannelCreate", guildID, data); err != nil {
		return nil, err
	}
	ch := &discordgo.Channel{
		ID: f.newIDLocked(), GuildID: guildID, Name: data.Name, Type: data.Type, Topic: data.Topic,
		ParentID: data.ParentID, PermissionOverwrites: data.PermissionOverwrites,
	}
	f.Channels[ch.ID] = ch
	c := *ch
	return &c, nil
}

func (f *Fake) ChannelEdit(_ context.Context, channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ChannelEdit", channelID, *data); err != nil {
		return nil, err
	}
	ch, ok := f.Channels[channelID]
	if !ok {
		return nil, &Error{Status: 404, Code: CodeUnknownChannel, Message: "Unknown Channel"}
	}
	if data.Name != "" {
		ch.Name = data.Name
	}
	if data.Topic != "" {
		ch.Topic = data.Topic
	}
	if data.ParentID != "" {
		ch.ParentID = data.ParentID
	}
	if data.PermissionOverwrites != nil {
		ch.PermissionOverwrites = data.PermissionOverwrites
	}
	c := *ch
	return &c, nil
}

func (f *Fake) ChannelDelete(_ context.Context, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ChannelDelete", channelID); err != nil {
		return err
	}
	if _, ok := f.Channels[channelID]; !ok {
		return &Error{Status: 404, Code: CodeUnknownChannel, Message: "Unknown Channel"}
	}
	delete(f.Channels, channelID)
	delete(f.Messages, channelID)
	return nil
}

func (f *Fake) ChannelMessages(_ context.Context, channelID string, limit int, afterID string) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ChannelMessages", channelID, limit, afterID); err != nil {
		return nil, err
	}
	var after []*discordgo.Message
	for _, m := range f.Messages[channelID] {
		if afterID == "" || compareIDs(m.ID, afterID) > 0 {
			after = append(after, m)
		}
	}
	// Discord returns the oldest `limit` messages after afterID, newest first.
	if len(after) > limit {
		after = after[:limit]
	}
	slices.Reverse(after)
	return after, nil
}

func (f *Fake) MessageSend(_ context.Context, channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("MessageSend", channelID, data); err != nil {
		return nil, err
	}
	return f.postLocked(channelID, data, "")
}

// MessageSendOnce is recorded (and fails) as MessageSend, so tests see one
// kind of post. A repeated nonce returns the first message, like Discord's
// enforce_nonce within its window.
func (f *Fake) MessageSendOnce(_ context.Context, channelID, content, nonce string) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := &discordgo.MessageSend{Content: content}
	if err := f.record("MessageSend", channelID, data); err != nil {
		return nil, err
	}
	if m, ok := f.nonces[channelID+"\x00"+nonce]; ok {
		return m, nil
	}
	return f.postLocked(channelID, data, nonce)
}

func (f *Fake) postLocked(channelID string, data *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
	if _, ok := f.Channels[channelID]; !ok {
		return nil, &Error{Status: 404, Code: CodeUnknownChannel, Message: "Unknown Channel"}
	}
	m := &discordgo.Message{
		ID: f.newIDLocked(), ChannelID: channelID, Content: data.Content, Embeds: data.Embeds,
		Components: data.Components, Author: f.BotUser, MessageReference: data.Reference,
	}
	f.Messages[channelID] = append(f.Messages[channelID], m)
	if nonce != "" {
		f.nonces[channelID+"\x00"+nonce] = m
	}
	if err := f.lost("MessageSend"); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *Fake) MessageEdit(_ context.Context, data *discordgo.MessageEdit) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("MessageEdit", data.Channel, data.ID, data); err != nil {
		return nil, err
	}
	for _, m := range f.Messages[data.Channel] {
		if m.ID != data.ID {
			continue
		}
		if data.Content != nil {
			m.Content = *data.Content
		}
		if data.Embeds != nil {
			m.Embeds = *data.Embeds
		}
		if data.Components != nil {
			m.Components = *data.Components
		}
		return m, nil
	}
	return nil, &Error{Status: 404, Code: CodeUnknownMessage, Message: "Unknown Message"}
}

func (f *Fake) ReactionAdd(_ context.Context, channelID, messageID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ReactionAdd", channelID, messageID, emoji); err != nil {
		return err
	}
	f.Reactions[messageID] = append(f.Reactions[messageID], emoji)
	return nil
}

// ReactionsOn returns the reactions the bot added to a message.
func (f *Fake) ReactionsOn(messageID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Reactions[messageID])
}

func (f *Fake) MessagePin(_ context.Context, channelID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("MessagePin", channelID, messageID); err != nil {
		return err
	}
	f.Pins[messageID] = true
	return nil
}

func (f *Fake) BulkOverwriteGuildCommands(_ context.Context, appID, guildID string, commands []*discordgo.ApplicationCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("BulkOverwriteGuildCommands", appID, guildID, len(commands)); err != nil {
		return err
	}
	f.Commands[guildID] = commands
	return nil
}

func (f *Fake) InteractionRespond(_ context.Context, interaction *discordgo.Interaction, resp *discordgo.InteractionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("InteractionRespond", interaction.ID, resp.Type); err != nil {
		return err
	}
	f.Interactions[interaction.ID] = append(f.Interactions[interaction.ID], resp)
	return nil
}

func (f *Fake) InteractionEdit(_ context.Context, interaction *discordgo.Interaction, edit *discordgo.WebhookEdit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("InteractionEdit", interaction.ID); err != nil {
		return err
	}
	f.Interactions[interaction.ID] = append(f.Interactions[interaction.ID], edit)
	return nil
}

// LastReply returns the text of the latest response or edit for an
// interaction: the edited content when deferred, or the response content.
func (f *Fake) LastReply(interactionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.Interactions[interactionID]
	for i := len(items) - 1; i >= 0; i-- {
		switch v := items[i].(type) {
		case *discordgo.WebhookEdit:
			if v.Content != nil {
				return *v.Content
			}
		case *discordgo.InteractionResponse:
			if v.Data != nil && v.Data.Content != "" {
				return v.Data.Content
			}
		}
	}
	return ""
}

// compareIDs orders snowflakes numerically.
func compareIDs(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CompareIDs orders Discord snowflakes numerically: negative when a < b.
func CompareIDs(a, b string) int { return compareIDs(a, b) }

// String renders a call for test failure messages.
func (c Call) String() string { return fmt.Sprintf("%s%v", c.Method, c.Args) }
