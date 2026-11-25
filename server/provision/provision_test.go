package provision

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/commands"
	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/discordapi"
	"kandev-plugin-discord/server/store"
)

const (
	guildID = "223456789012345678"
	appID   = "123456789012345678"
)

func input(viewer string) Input {
	return Input{
		Binding:      config.Binding{WorkspaceID: "ws", GuildID: guildID},
		Config:       config.Config{ApplicationID: appID, OperatorRoleName: "Kandev Operator", ViewerRoleName: viewer},
		BotUserID:    "900000000000000001",
		Commands:     commands.Definitions(),
		CommandsHash: commands.DefinitionsHash(appID),
		Now:          time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
	}
}

func TestProvisionCreatesEverythingOnceThenNothing(t *testing.T) {
	ctx := context.Background()
	api := discordapi.NewFake()
	st := store.New(store.NewMemoryHost())

	rec, err := Provision(ctx, api, st, input("Watchers"))
	require.NoError(t, err)
	assert.Len(t, api.CallsTo("GuildRoleCreate"), 2)
	assert.Len(t, api.CallsTo("GuildChannelCreate"), 3)
	assert.Len(t, api.CallsTo("BulkOverwriteGuildCommands"), 1)
	assert.Len(t, api.Commands[guildID], 5)
	assert.NotEmpty(t, rec.OperatorRoleID)
	assert.NotEmpty(t, rec.ViewerRoleID)
	require.Len(t, rec.TaskCategoryIDs, 1)
	require.Len(t, rec.ArchiveCategoryIDs, 1)
	assert.NotEmpty(t, rec.StatusChannelID)

	tasks := api.Channels[rec.TaskCategoryIDs[0]]
	assert.Equal(t, TaskCategoryName, tasks.Name)
	assert.True(t, SameOverwrites(tasks.PermissionOverwrites, TaskOverwrites(RolesOf(rec, "900000000000000001"))))
	everyone := tasks.PermissionOverwrites[0]
	assert.Equal(t, guildID, everyone.ID)
	assert.Equal(t, discordapi.PermViewChannel, everyone.Deny)

	stored, ok, err := st.Guild(ctx, "ws")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rec, stored)

	api.ResetCalls()
	again, err := Provision(ctx, api, st, input("Watchers"))
	require.NoError(t, err)
	assert.Equal(t, rec, again)
	assert.Equal(t, 0, api.Mutations(), "second run mutates nothing: %v", api.Calls())
}

func TestProvisionReusesExistingObjectsByName(t *testing.T) {
	ctx := context.Background()
	api := discordapi.NewFake()
	api.Roles[guildID] = []*discordgo.Role{{ID: "300000000000000001", Name: "Kandev Operator"}}
	cat := api.AddChannel(&discordgo.Channel{GuildID: guildID, Name: TaskCategoryName, Type: discordgo.ChannelTypeGuildCategory})
	st := store.New(store.NewMemoryHost())

	rec, err := Provision(ctx, api, st, input(""))
	require.NoError(t, err)
	assert.Equal(t, "300000000000000001", rec.OperatorRoleID)
	assert.Equal(t, []string{cat.ID}, rec.TaskCategoryIDs)
	assert.Empty(t, api.CallsTo("GuildRoleCreate"))
	assert.Len(t, api.CallsTo("ChannelEdit"), 1, "existing category gets the plugin's overwrites")
	assert.Len(t, api.CallsTo("GuildChannelCreate"), 2, "archive category and status channel")
}

func TestCommandsReRegisterOnlyWhenTheSetChanges(t *testing.T) {
	ctx := context.Background()
	api := discordapi.NewFake()
	st := store.New(store.NewMemoryHost())
	_, err := Provision(ctx, api, st, input(""))
	require.NoError(t, err)
	in := input("")
	in.CommandsHash = "changed"
	_, err = Provision(ctx, api, st, in)
	require.NoError(t, err)
	assert.Len(t, api.CallsTo("BulkOverwriteGuildCommands"), 2)
}
