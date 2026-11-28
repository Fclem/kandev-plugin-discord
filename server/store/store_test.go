package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelRoundTripMaintainsIndex(t *testing.T) {
	ctx := context.Background()
	h := NewMemoryHost()
	s := New(h)
	rec := Channel{
		TaskID: "t1", WorkspaceID: "ws", GuildID: "g", ChannelID: "100", Status: StatusActive,
		OutboundCursor: Cursor{CreatedAt: "2026-11-25T10:00:00Z", IDs: []string{"m1"}},
		SessionStates:  map[string]string{"s1": "RUNNING"},
		LastRenamedAt:  time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
	}
	require.NoError(t, s.PutChannel(ctx, rec, ""))

	got, ok, err := s.Channel(ctx, "t1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rec, got)

	ix, ok, err := s.ChannelIndex(ctx, "100")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, Index{TaskID: "t1", WorkspaceID: "ws", GuildID: "g"}, ix)

	// A recreated channel moves the index; a deleted one drops it.
	rec.ChannelID = "200"
	require.NoError(t, s.PutChannel(ctx, rec, "100"))
	_, ok, _ = s.ChannelIndex(ctx, "100")
	assert.False(t, ok)
	all, err := s.IndexedChannels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]Index{"200": {TaskID: "t1", WorkspaceID: "ws", GuildID: "g"}}, all)

	rec.Status = StatusDeleted
	require.NoError(t, s.PutChannel(ctx, rec, "200"))
	all, err = s.IndexedChannels(ctx)
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestEmptyPromptsDeleteTheKey(t *testing.T) {
	ctx := context.Background()
	h := NewMemoryHost()
	s := New(h)
	require.NoError(t, s.PutPrompts(ctx, "t1", Prompts{Items: []Prompt{{InteractionID: "i1"}}}))
	assert.Contains(t, h.Keys(), "task/t1/interaction_prompts")
	require.NoError(t, s.PutPrompts(ctx, "t1", Prompts{}))
	assert.NotContains(t, h.Keys(), "task/t1/interaction_prompts")
}
