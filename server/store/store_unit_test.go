package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordsRoundTripUnderTheirKeys(t *testing.T) {
	ctx := context.Background()
	h := NewMemoryHost()
	s := New(h)
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	g := Guild{GuildID: "g", OperatorRoleID: "r", TaskCategoryIDs: []string{"c1"}, ArchiveCategoryIDs: []string{"a1"}, ProvisionedAt: at}
	require.NoError(t, s.PutGuild(ctx, "ws", g))
	gotG, ok, err := s.Guild(ctx, "ws")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, g, gotG)

	a := StepApproval{SessionID: "s", FromStepID: "a", ToStepID: "b", Nonce: "n", MessageID: "m", PostedAt: at}
	require.NoError(t, s.PutStepApproval(ctx, "t1", a))
	gotA, ok, err := s.StepApproval(ctx, "t1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, a, gotA)
	require.NoError(t, s.DeleteStepApproval(ctx, "t1"))
	_, ok, err = s.StepApproval(ctx, "t1")
	require.NoError(t, err)
	assert.False(t, ok)

	hl := Health{GatewayState: "connected", IdentifyCount24h: 3, Bindings: []BindingHealth{{WorkspaceID: "ws", GuildID: "g", BotMember: true}}, CheckedAt: at}
	require.NoError(t, s.PutHealth(ctx, hl))
	gotH, ok, err := s.Health(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, hl, gotH)

	b := IdentifyBudget{Hours: map[string]int{"2026-11-25T10": 2}}
	require.NoError(t, s.PutIdentifyBudget(ctx, b))
	gotB, err := s.IdentifyBudget(ctx)
	require.NoError(t, err)
	assert.Equal(t, b, gotB)

	p := Prompts{Items: []Prompt{{InteractionID: "i", Kind: "permission", SessionID: "s", MessageID: "m", PostedAt: at}}}
	require.NoError(t, s.PutPrompts(ctx, "t1", p))
	gotP, err := s.Prompts(ctx, "t1")
	require.NoError(t, err)
	assert.Equal(t, p, gotP)

	assert.Equal(t, []string{
		"instance//health", "instance//identify_budget",
		"task/t1/interaction_prompts", "workspace/ws/guild",
	}, h.Keys())
}

func TestMissingRecordsAreZeroValues(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryHost())
	_, ok, err := s.Guild(ctx, "ws")
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = s.Health(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	b, err := s.IdentifyBudget(ctx)
	require.NoError(t, err)
	assert.Nil(t, b.Hours)
	p, err := s.Prompts(ctx, "t")
	require.NoError(t, err)
	assert.Empty(t, p.Items)
	creating, err := s.CreatingTasks(ctx)
	require.NoError(t, err)
	assert.Empty(t, creating)
}

func TestCreatingInventoryIsSeparateFromChannelIndex(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryHost())
	require.NoError(t, s.PutCreating(ctx, Index{TaskID: "t1", WorkspaceID: "ws", GuildID: "g"}))
	require.NoError(t, s.PutCreating(ctx, Index{TaskID: "t2", WorkspaceID: "ws", GuildID: "g"}))
	require.NoError(t, s.PutChannel(ctx, Channel{TaskID: "t3", WorkspaceID: "ws", GuildID: "g", ChannelID: "300", Status: StatusActive}, ""))

	creating, err := s.CreatingTasks(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]Index{
		"t1": {TaskID: "t1", WorkspaceID: "ws", GuildID: "g"},
		"t2": {TaskID: "t2", WorkspaceID: "ws", GuildID: "g"},
	}, creating)
	indexed, err := s.IndexedChannels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]Index{"300": {TaskID: "t3", WorkspaceID: "ws", GuildID: "g"}}, indexed)

	require.NoError(t, s.DeleteCreating(ctx, "t1"))
	creating, _ = s.CreatingTasks(ctx)
	assert.Len(t, creating, 1)
}

func TestChannelWithoutDiscordIDIsNotIndexed(t *testing.T) {
	ctx := context.Background()
	h := NewMemoryHost()
	s := New(h)
	require.NoError(t, s.PutChannel(ctx, Channel{TaskID: "t1", Status: StatusCreating}, ""))
	assert.Equal(t, []string{"task/t1/channel"}, h.Keys())
	// Same channel ID as before keeps the index entry.
	require.NoError(t, s.PutChannel(ctx, Channel{TaskID: "t1", ChannelID: "100", Status: StatusActive}, ""))
	require.NoError(t, s.PutChannel(ctx, Channel{TaskID: "t1", ChannelID: "100", Status: StatusArchived}, "100"))
	_, ok, err := s.ChannelIndex(ctx, "100")
	require.NoError(t, err)
	assert.True(t, ok)
}

// faultyHost fails selected StateHost operations.
type faultyHost struct {
	*MemoryHost
	getErr, setErr, deleteErr, listErr error
	raw                                map[string]any
}

func (f *faultyHost) GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	if f.raw != nil {
		return f.raw, true, nil
	}
	return f.MemoryHost.GetState(ctx, scope, scopeID, key)
}

func (f *faultyHost) SetState(ctx context.Context, scope, scopeID, key string, v map[string]any) error {
	if f.setErr != nil {
		return f.setErr
	}
	return f.MemoryHost.SetState(ctx, scope, scopeID, key, v)
}

func (f *faultyHost) DeleteState(ctx context.Context, scope, scopeID, key string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.MemoryHost.DeleteState(ctx, scope, scopeID, key)
}

func (f *faultyHost) ListState(ctx context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.MemoryHost.ListState(ctx, scope, scopeID)
}

func TestHostErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("host state unavailable")

	get := New(&faultyHost{MemoryHost: NewMemoryHost(), getErr: boom})
	_, ok, err := get.Channel(ctx, "t1")
	assert.ErrorIs(t, err, boom)
	assert.False(t, ok)

	list := New(&faultyHost{MemoryHost: NewMemoryHost(), listErr: boom})
	_, err = list.IndexedChannels(ctx)
	assert.ErrorIs(t, err, boom)

	set := New(&faultyHost{MemoryHost: NewMemoryHost(), setErr: boom})
	assert.ErrorIs(t, set.PutChannel(ctx, Channel{TaskID: "t1", ChannelID: "1"}, ""), boom)

	h := &faultyHost{MemoryHost: NewMemoryHost()}
	del := New(h)
	require.NoError(t, del.PutChannel(ctx, Channel{TaskID: "t1", ChannelID: "100", Status: StatusActive}, ""))
	h.deleteErr = boom
	moved := Channel{TaskID: "t1", ChannelID: "200", Status: StatusActive}
	assert.ErrorIs(t, del.PutChannel(ctx, moved, "100"), boom)
	got, _, _ := del.Channel(ctx, "t1")
	assert.Equal(t, "100", got.ChannelID, "the record is not rewritten when the old index cannot be dropped")
	assert.ErrorIs(t, del.PutChannel(ctx, Channel{TaskID: "t1", ChannelID: "100", Status: StatusDeleted}, "100"), boom)
}

func TestCorruptRecordIsReportedNotZeroed(t *testing.T) {
	ctx := context.Background()
	s := New(&faultyHost{MemoryHost: NewMemoryHost(), raw: map[string]any{"guild_id": 42}})
	_, ok, err := s.Guild(ctx, "ws")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "workspace/ws/guild")

	unencodable := New(&faultyHost{MemoryHost: NewMemoryHost(), raw: map[string]any{"x": make(chan int)}})
	_, _, err = unencodable.Health(ctx)
	assert.Error(t, err)
}

func TestMemoryHostListStateIsScoped(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryHost()
	require.NoError(t, m.SetState(ctx, "task", "t1", "b", map[string]any{"v": 1}))
	require.NoError(t, m.SetState(ctx, "task", "t1", "a", map[string]any{"v": 2}))
	require.NoError(t, m.SetState(ctx, "task", "t10", "c", map[string]any{"v": 3}))
	entries, err := m.ListState(ctx, "task", "t1")
	require.NoError(t, err)
	require.Len(t, entries, 2, "t10 is a different scope ID")
	assert.Equal(t, "a", entries[0].Key)
	assert.Equal(t, "b", entries[1].Key)
}
