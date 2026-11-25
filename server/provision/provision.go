// Package provision prepares a bound guild: operator and viewer roles, the
// task and archive categories with their permission overwrites, the
// #kandev-status channel, and the guild commands. It reuses what it created
// before, so a second run creates nothing.
package provision

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/store"
)

// Object names.
const (
	TaskCategoryName    = "Kandev Tasks"
	ArchiveCategoryName = "Kandev Archive"
	StatusChannelName   = "kandev-status"
)

// Overwrite bit sets.
const (
	botAllow = discordapi.PermViewChannel | discordapi.PermSendMessages | discordapi.PermReadMessageHistory |
		discordapi.PermEmbedLinks | discordapi.PermAddReactions | discordapi.PermPinMessages
	readAllow  = discordapi.PermViewChannel | discordapi.PermReadMessageHistory
	writeAllow = readAllow | discordapi.PermSendMessages | discordapi.PermAddReactions
	writeDeny  = discordapi.PermSendMessages | discordapi.PermAddReactions
)

// Roles identifies the IDs an overwrite set refers to.
type Roles struct {
	GuildID, OperatorRoleID, ViewerRoleID, BotUserID string
}

// RolesOf returns the overwrite inputs for a provisioned guild.
func RolesOf(g store.Guild, botUserID string) Roles {
	return Roles{GuildID: g.GuildID, OperatorRoleID: g.OperatorRoleID, ViewerRoleID: g.ViewerRoleID, BotUserID: botUserID}
}

// TaskOverwrites: hidden from @everyone; operators read and write; viewers
// read; the bot has what it needs.
func TaskOverwrites(r Roles) []*discordgo.PermissionOverwrite {
	return overwrites(r, writeAllow, 0)
}

// ReadOnlyOverwrites is TaskOverwrites with sending denied to operators,
// for archived channels and #kandev-status.
func ReadOnlyOverwrites(r Roles) []*discordgo.PermissionOverwrite {
	return overwrites(r, readAllow, writeDeny)
}

func overwrites(r Roles, operatorAllow, operatorDeny int64) []*discordgo.PermissionOverwrite {
	out := []*discordgo.PermissionOverwrite{
		{ID: r.GuildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordapi.PermViewChannel},
		{ID: r.BotUserID, Type: discordgo.PermissionOverwriteTypeMember, Allow: botAllow},
	}
	if r.OperatorRoleID != "" {
		out = append(out, &discordgo.PermissionOverwrite{ID: r.OperatorRoleID, Type: discordgo.PermissionOverwriteTypeRole, Allow: operatorAllow, Deny: operatorDeny})
	}
	if r.ViewerRoleID != "" {
		out = append(out, &discordgo.PermissionOverwrite{ID: r.ViewerRoleID, Type: discordgo.PermissionOverwriteTypeRole, Allow: readAllow, Deny: writeDeny})
	}
	return out
}

// SameOverwrites compares overwrite sets regardless of order.
func SameOverwrites(a, b []*discordgo.PermissionOverwrite) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.ContainsFunc(b, func(y *discordgo.PermissionOverwrite) bool {
			return x.ID == y.ID && x.Type == y.Type && x.Allow == y.Allow && x.Deny == y.Deny
		}) {
			return false
		}
	}
	return true
}

// Input is one provisioning run.
type Input struct {
	Binding      config.Binding
	Config       config.Config
	BotUserID    string
	Commands     []*discordgo.ApplicationCommand
	CommandsHash string
	Now          time.Time
}

// Provision creates or reuses every guild object and stores the record.
func Provision(ctx context.Context, api discordapi.API, st *store.Store, in Input) (store.Guild, error) {
	rec, _, err := st.Guild(ctx, in.Binding.WorkspaceID)
	if err != nil {
		return rec, err
	}
	if rec.GuildID != in.Binding.GuildID {
		rec = store.Guild{GuildID: in.Binding.GuildID}
	}
	roles, err := api.GuildRoles(ctx, rec.GuildID)
	if err != nil {
		return rec, fmt.Errorf("listing roles: %w", err)
	}
	if rec.OperatorRoleID, err = ensureRole(ctx, api, rec.GuildID, roles, rec.OperatorRoleID, in.Config.OperatorRoleName); err != nil {
		return rec, err
	}
	rec.ViewerRoleID = ""
	if in.Config.ViewerRoleName != "" {
		if rec.ViewerRoleID, err = ensureRole(ctx, api, rec.GuildID, roles, rec.ViewerRoleID, in.Config.ViewerRoleName); err != nil {
			return rec, err
		}
	}

	channels, err := api.GuildChannels(ctx, rec.GuildID)
	if err != nil {
		return rec, fmt.Errorf("listing channels: %w", err)
	}
	r := RolesOf(rec, in.BotUserID)
	if rec.TaskCategoryIDs, err = ensureCategories(ctx, api, rec.GuildID, channels, rec.TaskCategoryIDs, TaskCategoryName, TaskOverwrites(r)); err != nil {
		return rec, err
	}
	if rec.ArchiveCategoryIDs, err = ensureCategories(ctx, api, rec.GuildID, channels, rec.ArchiveCategoryIDs, ArchiveCategoryName, ReadOnlyOverwrites(r)); err != nil {
		return rec, err
	}
	if rec.StatusChannelID, err = ensureStatusChannel(ctx, api, rec.GuildID, channels, rec.StatusChannelID, ReadOnlyOverwrites(r)); err != nil {
		return rec, err
	}

	if rec.CommandsHash != in.CommandsHash {
		if err := api.BulkOverwriteGuildCommands(ctx, in.Config.ApplicationID, rec.GuildID, in.Commands); err != nil {
			return rec, fmt.Errorf("registering commands: %w", err)
		}
		rec.CommandsHash = in.CommandsHash
	}
	if rec.ProvisionedAt.IsZero() {
		rec.ProvisionedAt = in.Now
	}
	return rec, st.PutGuild(ctx, in.Binding.WorkspaceID, rec)
}

func ensureRole(ctx context.Context, api discordapi.API, guildID string, roles []*discordgo.Role, storedID, name string) (string, error) {
	for _, r := range roles {
		if r.ID == storedID {
			return r.ID, nil
		}
	}
	for _, r := range roles {
		if r.Name == name {
			return r.ID, nil
		}
	}
	no := false
	perms := int64(0)
	created, err := api.GuildRoleCreate(ctx, guildID, &discordgo.RoleParams{Name: name, Mentionable: &no, Hoist: &no, Permissions: &perms})
	if err != nil {
		return "", fmt.Errorf("creating role %q: %w", name, err)
	}
	return created.ID, nil
}

// ensureCategories keeps the stored categories that still exist (rollover
// categories included), or finds or creates the first one, and corrects
// their overwrites.
func ensureCategories(ctx context.Context, api discordapi.API, guildID string, channels []*discordgo.Channel, stored []string, name string, want []*discordgo.PermissionOverwrite) ([]string, error) {
	var kept []string
	for _, id := range stored {
		if ch := find(channels, func(c *discordgo.Channel) bool { return c.ID == id && c.Type == discordgo.ChannelTypeGuildCategory }); ch != nil {
			if err := fixOverwrites(ctx, api, ch, want); err != nil {
				return nil, err
			}
			kept = append(kept, id)
		}
	}
	if len(kept) > 0 {
		return kept, nil
	}
	if ch := find(channels, func(c *discordgo.Channel) bool { return c.Name == name && c.Type == discordgo.ChannelTypeGuildCategory }); ch != nil {
		return []string{ch.ID}, fixOverwrites(ctx, api, ch, want)
	}
	ch, err := CreateCategory(ctx, api, guildID, name, want)
	if err != nil {
		return nil, err
	}
	return []string{ch.ID}, nil
}

// CreateCategory creates one category with overwrites.
func CreateCategory(ctx context.Context, api discordapi.API, guildID, name string, want []*discordgo.PermissionOverwrite) (*discordgo.Channel, error) {
	ch, err := api.GuildChannelCreate(ctx, guildID, discordgo.GuildChannelCreateData{
		Name: name, Type: discordgo.ChannelTypeGuildCategory, PermissionOverwrites: want,
	})
	if err != nil {
		return nil, fmt.Errorf("creating category %q: %w", name, err)
	}
	return ch, nil
}

func ensureStatusChannel(ctx context.Context, api discordapi.API, guildID string, channels []*discordgo.Channel, storedID string, want []*discordgo.PermissionOverwrite) (string, error) {
	ch := find(channels, func(c *discordgo.Channel) bool { return storedID != "" && c.ID == storedID })
	if ch == nil {
		ch = find(channels, func(c *discordgo.Channel) bool {
			return c.Name == StatusChannelName && c.Type == discordgo.ChannelTypeGuildText
		})
	}
	if ch != nil {
		return ch.ID, fixOverwrites(ctx, api, ch, want)
	}
	created, err := api.GuildChannelCreate(ctx, guildID, discordgo.GuildChannelCreateData{
		Name: StatusChannelName, Type: discordgo.ChannelTypeGuildText, PermissionOverwrites: want,
		Topic: "Kandev Discord bridge status: connections, provisioning, and capacity.",
	})
	if err != nil {
		return "", fmt.Errorf("creating #%s: %w", StatusChannelName, err)
	}
	return created.ID, nil
}

func fixOverwrites(ctx context.Context, api discordapi.API, ch *discordgo.Channel, want []*discordgo.PermissionOverwrite) error {
	if SameOverwrites(ch.PermissionOverwrites, want) {
		return nil
	}
	if _, err := api.ChannelEdit(ctx, ch.ID, &discordgo.ChannelEdit{PermissionOverwrites: want}); err != nil {
		return fmt.Errorf("updating permissions of %q: %w", ch.Name, err)
	}
	return nil
}

func find(channels []*discordgo.Channel, match func(*discordgo.Channel) bool) *discordgo.Channel {
	for _, c := range channels {
		if match(c) {
			return c
		}
	}
	return nil
}
