// Package store keeps the plugin's durable records in Kandev Host state
// (plugin-namespaced JSON, backed up, removed on uninstall). The layout
// follows docs/design/system-design.md "Data model".
//
// The plugin is the only writer. Callers serialize writes per task (one
// worker per task) because Host state has no compare-and-swap.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
)

// StateHost is the Host state subset the store uses.
type StateHost interface {
	GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error)
	SetState(ctx context.Context, scope, scopeID, key string, value map[string]any) error
	DeleteState(ctx context.Context, scope, scopeID, key string) error
	ListState(ctx context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error)
}

// Host state scopes and keys.
const (
	scopeInstance  = "instance"
	scopeWorkspace = "workspace"
	scopeTask      = "task"

	keyGuild        = "guild"
	keyChannel      = "channel"
	keyPrompts      = "interaction_prompts"
	keyStepApproval = "step_approval"
	keyHealth       = "health"
	keyIdentify     = "identify_budget"
	indexPrefix     = "channel:"
)

// Channel record statuses (system design "State machine").
const (
	StatusCreating        = "creating"
	StatusActive          = "active"
	StatusArchived        = "archived"
	StatusDeleted         = "deleted"
	StatusCapacityBlocked = "capacity_blocked"
	StatusFailed          = "failed"
)

// Guild is workspace/<id>/guild: what provisioning created in one guild.
type Guild struct {
	GuildID            string    `json:"guild_id"`
	OperatorRoleID     string    `json:"operator_role_id"`
	ViewerRoleID       string    `json:"viewer_role_id,omitempty"`
	StatusChannelID    string    `json:"status_channel_id"`
	TaskCategoryIDs    []string  `json:"task_category_ids"`
	ArchiveCategoryIDs []string  `json:"archive_category_ids"`
	CommandsHash       string    `json:"commands_hash"`
	ProvisionedAt      time.Time `json:"provisioned_at"`
}

// Cursor is the outbound position: the newest posted message's timestamp
// and every posted message ID at exactly that timestamp (tie-break).
//
// PartialID, PartialChannelID, and PartsSent record a message whose chunks
// were only partly posted to that channel, so a retry resumes after the
// last delivered chunk. Sending marks that part PartsSent was being sent
// when the record was saved: its outcome is unknown, and a retry first
// looks for it after LastPostID (the last part known to be posted).
type Cursor struct {
	CreatedAt        string   `json:"created_at,omitempty"`
	IDs              []string `json:"ids,omitempty"`
	PartialID        string   `json:"partial_id,omitempty"`
	PartialChannelID string   `json:"partial_channel_id,omitempty"`
	PartsSent        int      `json:"parts_sent,omitempty"`
	Sending          bool     `json:"sending,omitempty"`
	LastPostID       string   `json:"last_post_id,omitempty"`
}

// Creation is what a `creating` record asked Discord for, so a retry can
// recognize the channel a lost write left behind even after a rename.
type Creation struct {
	Name  string    `json:"name"`
	Topic string    `json:"topic"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
}

// Channel is task/<id>/channel: the task's Discord channel and sync state.
type Channel struct {
	TaskID          string            `json:"task_id"`
	WorkspaceID     string            `json:"workspace_id"`
	GuildID         string            `json:"guild_id"`
	ChannelID       string            `json:"channel_id,omitempty"`
	Status          string            `json:"status"`
	SyncedTitle     string            `json:"synced_title,omitempty"`
	SyncedTopic     string            `json:"synced_topic,omitempty"`
	RolesKey        string            `json:"roles_key,omitempty"`
	LastRenamedAt   time.Time         `json:"last_renamed_at,omitzero"`
	CardMessageID   string            `json:"card_message_id,omitempty"`
	CardHash        string            `json:"card_hash,omitempty"`
	CardPinned      bool              `json:"card_pinned,omitempty"`
	CardEditedAt    time.Time         `json:"card_edited_at,omitzero"`
	OutboundCursor  Cursor            `json:"outbound_cursor"`
	InboundCursor   string            `json:"inbound_cursor,omitempty"`
	InboundInflight string            `json:"inbound_inflight,omitempty"`
	SessionStates   map[string]string `json:"session_states,omitempty"`
	ClosedAt        time.Time         `json:"closed_at,omitzero"`
	DeleteAfter     time.Time         `json:"delete_after,omitzero"`
	LastError       string            `json:"last_error,omitempty"`
	Creation        *Creation         `json:"creation,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// Prompt is one posted interaction prompt.
type Prompt struct {
	InteractionID string    `json:"interaction_id"`
	Kind          string    `json:"kind"`
	SessionID     string    `json:"session_id"`
	MessageID     string    `json:"discord_message_id"`
	PostedAt      time.Time `json:"posted_at"`
}

// Prompts is task/<id>/interaction_prompts. Host Interactions() is the
// source of truth; this only remembers what was posted.
type Prompts struct {
	Items []Prompt `json:"items"`
}

// StepApproval is task/<id>/step_approval: at most one posted prompt.
// Nonce identifies the posted prompt in its button, so a button of an
// earlier prompt never approves a later visit to the same step.
type StepApproval struct {
	SessionID  string    `json:"session_id"`
	FromStepID string    `json:"from_step_id"`
	ToStepID   string    `json:"to_step_id"`
	Nonce      string    `json:"nonce,omitempty"`
	MessageID  string    `json:"discord_message_id"`
	PostedAt   time.Time `json:"posted_at"`
}

// BindingHealth is one binding's last observed state.
type BindingHealth struct {
	WorkspaceID string `json:"workspace_id"`
	GuildID     string `json:"guild_id"`
	BotMember   bool   `json:"bot_member"`
	Provisioned bool   `json:"provisioned"`
	InviteURL   string `json:"invite_url,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Health is instance//health, served by connection.status without Discord
// calls. It never holds token material.
type Health struct {
	ConfigError      string          `json:"config_error,omitempty"`
	GatewayState     string          `json:"gateway_state"`
	LastCloseCode    int             `json:"last_close_code,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	IdentifyCount24h int             `json:"identify_count_24h"`
	Bindings         []BindingHealth `json:"bindings"`
	CheckedAt        time.Time       `json:"checked_at"`
}

// IdentifyBudget is instance//identify_budget: READY counts per UTC hour.
type IdentifyBudget struct {
	Hours map[string]int `json:"hours"`
}

// Index is instance//channel:<channel id>, the reverse channel lookup.
type Index struct {
	TaskID      string `json:"task_id"`
	WorkspaceID string `json:"workspace_id"`
	GuildID     string `json:"guild_id"`
}

// Store reads and writes typed records.
type Store struct{ h StateHost }

// New returns a Store over h.
func New(h StateHost) *Store { return &Store{h: h} }

func (s *Store) get(ctx context.Context, scope, scopeID, key string, out any) (bool, error) {
	raw, found, err := s.h.GetState(ctx, scope, scopeID, key)
	if err != nil || !found {
		return false, err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("decoding %s/%s/%s: %w", scope, scopeID, key, err)
	}
	return true, nil
}

func (s *Store) put(ctx context.Context, scope, scopeID, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	return s.h.SetState(ctx, scope, scopeID, key, m)
}

// Guild returns a workspace's guild record.
func (s *Store) Guild(ctx context.Context, workspaceID string) (Guild, bool, error) {
	var g Guild
	ok, err := s.get(ctx, scopeWorkspace, workspaceID, keyGuild, &g)
	return g, ok, err
}

// PutGuild stores a workspace's guild record.
func (s *Store) PutGuild(ctx context.Context, workspaceID string, g Guild) error {
	return s.put(ctx, scopeWorkspace, workspaceID, keyGuild, g)
}

// Channel returns a task's channel record.
func (s *Store) Channel(ctx context.Context, taskID string) (Channel, bool, error) {
	var c Channel
	ok, err := s.get(ctx, scopeTask, taskID, keyChannel, &c)
	return c, ok, err
}

// PutChannel stores a task's channel record and keeps the reverse index:
// a record with a channel ID is indexed, a deleted one is not.
func (s *Store) PutChannel(ctx context.Context, c Channel, previousChannelID string) error {
	if previousChannelID != "" && previousChannelID != c.ChannelID {
		if err := s.h.DeleteState(ctx, scopeInstance, "", indexPrefix+previousChannelID); err != nil {
			return err
		}
	}
	if err := s.put(ctx, scopeTask, c.TaskID, keyChannel, c); err != nil {
		return err
	}
	if c.ChannelID == "" {
		return nil
	}
	if c.Status == StatusDeleted {
		return s.h.DeleteState(ctx, scopeInstance, "", indexPrefix+c.ChannelID)
	}
	return s.put(ctx, scopeInstance, "", indexPrefix+c.ChannelID, Index{TaskID: c.TaskID, WorkspaceID: c.WorkspaceID, GuildID: c.GuildID})
}

// ChannelIndex resolves a Discord channel to its task.
func (s *Store) ChannelIndex(ctx context.Context, channelID string) (Index, bool, error) {
	var ix Index
	ok, err := s.get(ctx, scopeInstance, "", indexPrefix+channelID, &ix)
	return ix, ok, err
}

// IndexedChannels lists every indexed channel ID with its task.
func (s *Store) IndexedChannels(ctx context.Context) (map[string]Index, error) {
	entries, err := s.h.ListState(ctx, scopeInstance, "")
	if err != nil {
		return nil, err
	}
	out := map[string]Index{}
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Key, indexPrefix)
		if !ok {
			continue
		}
		out[id] = Index{TaskID: str(e.Value["task_id"]), WorkspaceID: str(e.Value["workspace_id"]), GuildID: str(e.Value["guild_id"])}
	}
	return out, nil
}

// Prompts returns a task's posted interaction prompts.
func (s *Store) Prompts(ctx context.Context, taskID string) (Prompts, error) {
	var p Prompts
	_, err := s.get(ctx, scopeTask, taskID, keyPrompts, &p)
	return p, err
}

// PutPrompts stores a task's prompts, deleting the key when empty.
func (s *Store) PutPrompts(ctx context.Context, taskID string, p Prompts) error {
	if len(p.Items) == 0 {
		return s.h.DeleteState(ctx, scopeTask, taskID, keyPrompts)
	}
	return s.put(ctx, scopeTask, taskID, keyPrompts, p)
}

// StepApproval returns a task's posted step approval prompt.
func (s *Store) StepApproval(ctx context.Context, taskID string) (StepApproval, bool, error) {
	var a StepApproval
	ok, err := s.get(ctx, scopeTask, taskID, keyStepApproval, &a)
	return a, ok, err
}

// PutStepApproval stores a task's step approval prompt.
func (s *Store) PutStepApproval(ctx context.Context, taskID string, a StepApproval) error {
	return s.put(ctx, scopeTask, taskID, keyStepApproval, a)
}

// DeleteStepApproval forgets a task's step approval prompt.
func (s *Store) DeleteStepApproval(ctx context.Context, taskID string) error {
	return s.h.DeleteState(ctx, scopeTask, taskID, keyStepApproval)
}

// Health returns the cached health record.
func (s *Store) Health(ctx context.Context) (Health, bool, error) {
	var h Health
	ok, err := s.get(ctx, scopeInstance, "", keyHealth, &h)
	return h, ok, err
}

// PutHealth stores the cached health record.
func (s *Store) PutHealth(ctx context.Context, h Health) error {
	return s.put(ctx, scopeInstance, "", keyHealth, h)
}

// IdentifyBudget returns the persisted identify counter.
func (s *Store) IdentifyBudget(ctx context.Context) (IdentifyBudget, error) {
	var b IdentifyBudget
	_, err := s.get(ctx, scopeInstance, "", keyIdentify, &b)
	return b, err
}

// PutIdentifyBudget stores the identify counter.
func (s *Store) PutIdentifyBudget(ctx context.Context, b IdentifyBudget) error {
	return s.put(ctx, scopeInstance, "", keyIdentify, b)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// MemoryHost is an in-memory StateHost for tests.
type MemoryHost struct {
	mu   sync.Mutex
	data map[string]map[string]any
}

// NewMemoryHost returns an empty MemoryHost.
func NewMemoryHost() *MemoryHost { return &MemoryHost{data: map[string]map[string]any{}} }

func memKey(scope, scopeID, key string) string { return scope + "\x00" + scopeID + "\x00" + key }

func (m *MemoryHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[memKey(scope, scopeID, key)]
	return v, ok, nil
}

func (m *MemoryHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[memKey(scope, scopeID, key)] = value
	return nil
}

func (m *MemoryHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, memKey(scope, scopeID, key))
	return nil
}

func (m *MemoryHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := scope + "\x00" + scopeID + "\x00"
	var out []pluginsdk.StateEntry
	for k, v := range m.data {
		if key, ok := strings.CutPrefix(k, prefix); ok {
			out = append(out, pluginsdk.StateEntry{Key: key, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Keys lists every stored key (tests).
func (m *MemoryHost) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		out = append(out, strings.ReplaceAll(k, "\x00", "/"))
	}
	sort.Strings(out)
	return out
}
