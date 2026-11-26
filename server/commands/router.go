package commands

import (
	"context"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-discord/server/authz"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/kandev"
	"kandev-plugin-discord/server/store"
)

// Env is what command handlers need from the bridge.
type Env struct {
	API    discordapi.API
	Host   kandev.Host
	Config config.Config
	Store  *store.Store
	// Guild resolves a bound, provisioned guild.
	Guild func(guildID string) (store.Guild, config.Binding, bool)
	// QueueCount is a session's cached queue depth.
	QueueCount func(sessionID string) int
	// StepName resolves a task's workflow step name.
	StepName func(ctx context.Context, task *pluginsdk.Task) string
	// OnTask runs fn on the task's queue (serialized with its sync) and
	// waits for it.
	OnTask func(taskID string, fn func(ctx context.Context))
	// Refresh queues a sync of the task (prompts, card).
	Refresh func(taskID string)
}

// Router answers Discord interactions.
type Router struct{ Env }

// call is one interaction being answered.
type call struct {
	*Router
	i        *discordgo.Interaction
	guild    store.Guild
	binding  config.Binding
	userID   string
	userName string
	operator bool
}

// Handle answers one interaction. Commands and buttons are deferred
// (ephemerally) before any Kandev call; autocomplete answers directly.
func (r *Router) Handle(ctx context.Context, i *discordgo.Interaction) {
	guild, binding, ok := r.Guild(i.GuildID)
	if !ok {
		if i.Type != discordgo.InteractionApplicationCommandAutocomplete {
			r.respond(ctx, i, "This server is not bound to a Kandev workspace, or the bridge is still starting.")
		}
		return
	}
	c := &call{Router: r, i: i, guild: guild, binding: binding,
		userID: authz.UserID(i.Member, i.User), userName: authz.DisplayName(i.Member, i.User)}
	c.operator = authz.IsOperator(r.Config, guild.OperatorRoleID, i.Member, c.userID)
	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		c.autocomplete(ctx)
		return
	}
	if err := r.API.InteractionRespond(ctx, i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		log.Printf("discord: deferring interaction %s: %v", i.ID, err)
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		c.command(ctx)
	case discordgo.InteractionMessageComponent:
		c.button(ctx)
	}
}

func (c *call) command(ctx context.Context) {
	data := c.i.ApplicationCommandData()
	if data.Name == CmdLink {
		c.link(ctx, optionString(data.Options, OptCode))
		return
	}
	taskID, ok := c.taskOfChannel(ctx)
	if !ok {
		c.reply(ctx, "Commands work only in Kandev task channels.")
		return
	}
	if data.Name != CmdStatus && !c.requireOpen(ctx, taskID) {
		return
	}
	switch data.Name {
	case CmdStatus:
		c.status(ctx, taskID)
	case CmdStop:
		scope := optionString(data.Options, OptScope)
		if scope == "" {
			scope = ScopeTurn
		}
		c.stop(ctx, taskID, scope)
	case CmdApprove:
		c.approve(ctx, taskID, optionString(data.Options, OptRequest), true)
	case CmdDeny:
		c.approve(ctx, taskID, optionString(data.Options, OptRequest), false)
	default:
		c.reply(ctx, "Unknown command.")
	}
}

// taskOfChannel maps the interaction's channel to its task channel (open
// or archived).
func (c *call) taskOfChannel(ctx context.Context) (string, bool) {
	ix, ok, err := c.Store.ChannelIndex(ctx, c.i.ChannelID)
	if err != nil || !ok || ix.WorkspaceID != c.binding.WorkspaceID || ix.GuildID != c.i.GuildID {
		return "", false
	}
	return ix.TaskID, true
}

// requireOpen admits actions on a task only from its active channel: an
// archived channel's leftover buttons and commands change nothing.
func (c *call) requireOpen(ctx context.Context, taskID string) bool {
	rec, found, err := c.Store.Channel(ctx, taskID)
	switch {
	case err != nil:
		c.reply(ctx, "Kandev could not check this channel right now; try again.")
		return false
	case !found || rec.Status != store.StatusActive || rec.ChannelID != c.i.ChannelID:
		c.reply(ctx, "This channel is closed; act on the task in Kandev"+c.linkSuffix(taskID))
		return false
	}
	return true
}

// maxReply is Discord's message content limit (characters).
const maxReply = 2000

// reply replaces the deferred ephemeral response. Text beyond Discord's
// limit is cut at a line boundary with a marker, so the edit never fails.
func (c *call) reply(ctx context.Context, text string) {
	text = clip(text, maxReply)
	if err := c.API.InteractionEdit(ctx, c.i, &discordgo.WebhookEdit{Content: &text}); err != nil {
		log.Printf("discord: answering interaction %s: %v", c.i.ID, err)
	}
}

// clip shortens text to at most limit characters, preferring a line break,
// and marks the cut.
func clip(text string, limit int) string {
	const marker = "\n… (more in Kandev)"
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	cut := string(r[:limit-len([]rune(marker))])
	if i := strings.LastIndex(cut, "\n"); i > len(cut)/2 {
		cut = cut[:i]
	}
	return cut + marker
}

// respond answers an interaction immediately and ephemerally.
func (r *Router) respond(ctx context.Context, i *discordgo.Interaction, text string) {
	if err := r.API.InteractionRespond(ctx, i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		log.Printf("discord: answering interaction %s: %v", i.ID, err)
	}
}

func (c *call) requireOperator(ctx context.Context) bool {
	if c.operator {
		return true
	}
	c.reply(ctx, "Only members with the operator role can do that.")
	return false
}

func optionString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name && o.Type == discordgo.ApplicationCommandOptionString {
			return o.StringValue()
		}
	}
	return ""
}
