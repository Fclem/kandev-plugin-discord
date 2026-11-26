package bridge

import (
	"context"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/config"
	"kandev-plugin-discord/server/store"
)

func (h *harness) reconcile(t *testing.T) {
	t.Helper()
	h.b.reconcileWorkspace(context.Background(), config.Binding{WorkspaceID: wsID, GuildID: guildID})
	h.wait()
}

func TestReadyReconcileCreatesChannelsMissedWhileDown(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "One")
	h.addTask("2", "Two")
	h.addTask("3", "Three")
	archived := "2026-11-20T00:00:00Z"
	h.host.Tasks["3"].ArchivedAt = &archived

	h.ready(t) // provisioning, then reconcile
	assert.Equal(t, store.StatusActive, h.channel(t, "1").Status)
	assert.Equal(t, store.StatusActive, h.channel(t, "2").Status)
	_, found, err := h.st.Channel(context.Background(), "3")
	require.NoError(t, err)
	assert.False(t, found, "archived task without a channel gets none")
	assert.Len(t, h.api.CallsTo("GuildChannelCreate"), 2+3, "two task channels after provisioning's three")

	h.api.ResetCalls()
	h.reconcile(t)
	assert.Equal(t, 0, h.api.Mutations(), "second run makes no Discord mutation: %v", h.api.Calls())
}

func TestReconcileClosesDeletedTasksAndRecreatesDeletedChannels(t *testing.T) {
	h := newHarness(t)
	h.addTask("1", "Gone")
	h.addTask("2", "Lost channel")
	h.ready(t)
	one, two := h.channel(t, "1"), h.channel(t, "2")

	delete(h.host.Tasks, "1")
	delete(h.api.Channels, two.ChannelID)
	h.b.onChannelDelete(context.Background(), &discordgo.Channel{ID: two.ChannelID})
	h.wait()
	h.reconcile(t)

	assert.Equal(t, store.StatusArchived, h.channel(t, "1").Status, "deleted task closes its channel")
	assert.Equal(t, h.b.guilds[wsID].ArchiveCategoryIDs[0], h.api.Channels[one.ChannelID].ParentID)
	recreated := h.channel(t, "2")
	assert.Equal(t, store.StatusActive, recreated.Status)
	assert.NotEqual(t, two.ChannelID, recreated.ChannelID)
}

func TestSchedulerRunsOneKeyInOrder(t *testing.T) {
	s := NewScheduler(context.Background())
	var mu sync.Mutex
	var got []int
	for i := range 50 {
		s.Do("k", func(context.Context) { mu.Lock(); got = append(got, i); mu.Unlock() })
	}
	s.Wait()
	require.Len(t, got, 50)
	for i, v := range got {
		assert.Equal(t, i, v)
	}
}
