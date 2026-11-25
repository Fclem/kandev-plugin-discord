// Package gateway supervises the Discord gateway websocket. discordgo's own
// reconnect loop is disabled: the supervisor reopens with jittered backoff,
// stops on fatal close codes, honors the REST 401 halt, and enforces the
// identify budget (Discord resets the token after 1000 identifies a day).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"kandev-plugin-discord/server/store"
)

// Intents the plugin needs; MESSAGE_CONTENT is privileged.
const Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

// IdentifyLimit is the plugin's cap per rolling 24 h, below Discord's 1000.
const IdentifyLimit = 900

// Gateway states reported to the health record.
const (
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateReconnecting = "reconnecting"
	StateFatal        = "fatal"
	StateRefused      = "identify_budget_exhausted"
	StateHalted       = "token_rejected"
	StateStopped      = "stopped"
)

// fatalReasons are close codes that retrying cannot fix.
var fatalReasons = map[int]string{
	4004: "Discord rejected the bot token (close code 4004): reset the token and save the settings",
	4010: "invalid shard (close code 4010)",
	4011: "sharding required (close code 4011)",
	4012: "invalid gateway API version (close code 4012)",
	4013: "invalid intents (close code 4013)",
	4014: "disallowed intents (close code 4014): enable the Message Content intent in the Developer Portal",
}

// Conn is the websocket the supervisor drives; *discordgo.Session fits.
type Conn interface {
	Open() error
	Close() error
}

// Budget counts identifies in a rolling 24 h window.
type Budget interface {
	Count(ctx context.Context, now time.Time) (int, error)
	Record(ctx context.Context, now time.Time) error
}

// State is one supervisor status update. Reason never contains the token.
type State struct {
	Name          string
	CloseCode     int
	Reason        string
	Identifies24h int
}

// Supervisor keeps one gateway connection open.
type Supervisor struct {
	Conn   Conn
	Budget Budget
	// Halted reports a REST 401 (discordapi.Client.Halted).
	Halted func() bool
	// Report receives every state change.
	Report func(State)

	MinBackoff, MaxBackoff, BudgetRetry time.Duration
	Now                                 func() time.Time
	After                               func(time.Duration) <-chan time.Time

	once         sync.Once
	disconnected chan struct{}
}

func (s *Supervisor) init() {
	s.once.Do(func() {
		s.disconnected = make(chan struct{}, 1)
		if s.MinBackoff == 0 {
			s.MinBackoff = time.Second
		}
		if s.MaxBackoff == 0 {
			s.MaxBackoff = 2 * time.Minute
		}
		if s.BudgetRetry == 0 {
			s.BudgetRetry = 10 * time.Minute
		}
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.After == nil {
			s.After = time.After
		}
		if s.Halted == nil {
			s.Halted = func() bool { return false }
		}
		if s.Report == nil {
			s.Report = func(State) {}
		}
	})
}

// Disconnected tells the supervisor the websocket closed (discordgo
// Disconnect event). It never blocks.
func (s *Supervisor) Disconnected() {
	s.init()
	select {
	case s.disconnected <- struct{}{}:
	default:
	}
}

// Identified records one successful IDENTIFY (discordgo Ready event).
func (s *Supervisor) Identified(ctx context.Context) {
	s.init()
	if err := s.Budget.Record(ctx, s.Now()); err != nil {
		s.Report(State{Name: StateConnected, Reason: "identify counter not saved: " + err.Error()})
	}
}

// Run blocks until ctx ends or a fatal condition stops the gateway.
func (s *Supervisor) Run(ctx context.Context) State {
	s.init()
	attempt := 0
	for {
		if ctx.Err() != nil {
			return s.stop(StateStopped, 0, "")
		}
		if s.Halted() {
			return s.stop(StateHalted, 0, "Discord rejected the bot token (HTTP 401): reset the token and save the settings")
		}
		count, err := s.Budget.Count(ctx, s.Now())
		if err != nil {
			s.Report(State{Name: StateReconnecting, Reason: "identify counter unavailable: " + err.Error()})
			s.sleep(ctx, s.backoff(attempt))
			attempt++
			continue
		}
		if count >= IdentifyLimit {
			s.Report(State{Name: StateRefused, Identifies24h: count,
				Reason: fmt.Sprintf("%d identifies in the last 24 h; waiting below %d", count, IdentifyLimit)})
			s.sleep(ctx, s.BudgetRetry)
			continue
		}
		s.drainDisconnect()
		s.Report(State{Name: StateConnecting, Identifies24h: count})
		if err := s.Conn.Open(); err != nil {
			if code := closeCode(err); fatalReasons[code] != "" {
				_ = s.Conn.Close()
				return s.stop(StateFatal, code, fatalReasons[code])
			}
			s.Report(State{Name: StateReconnecting, Identifies24h: count, Reason: openReason(err)})
			s.sleep(ctx, s.backoff(attempt))
			attempt++
			continue
		}
		attempt = 0
		s.Report(State{Name: StateConnected, Identifies24h: count})
		select {
		case <-ctx.Done():
			_ = s.Conn.Close()
			return s.stop(StateStopped, 0, "")
		case <-s.disconnected:
			s.Report(State{Name: StateReconnecting, Reason: "gateway connection closed"})
			s.sleep(ctx, s.backoff(0))
		}
	}
}

func (s *Supervisor) stop(name string, code int, reason string) State {
	st := State{Name: name, CloseCode: code, Reason: reason}
	s.Report(st)
	return st
}

func (s *Supervisor) drainDisconnect() {
	select {
	case <-s.disconnected:
	default:
	}
}

// sleep waits d or until ctx ends; it reports whether ctx is still live.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.After(d):
		return true
	}
}

// backoff is exponential from MinBackoff, capped, with ±25 % jitter.
func (s *Supervisor) backoff(attempt int) time.Duration {
	d := s.MinBackoff
	for i := 0; i < attempt && d < s.MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, s.MaxBackoff)
	jitter := time.Duration(rand.Int64N(int64(d)/2+1)) - d/4
	return d + jitter
}

func closeCode(err error) int {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return 0
}

func openReason(err error) string {
	if code := closeCode(err); code != 0 {
		return fmt.Sprintf("gateway closed with code %d", code)
	}
	return "gateway connection failed: " + err.Error()
}

// StoreBudget persists identify counts in hourly buckets.
type StoreBudget struct {
	mu sync.Mutex
	St *store.Store
}

const hourLayout = "2006-01-02T15"

// Count sums the buckets of the last 24 hours, including the current one.
func (b *StoreBudget) Count(ctx context.Context, now time.Time) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.St.IdentifyBudget(ctx)
	if err != nil {
		return 0, err
	}
	return countSince(rec, now), nil
}

// Record adds one identify to the current hour and prunes older buckets.
func (b *StoreBudget) Record(ctx context.Context, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.St.IdentifyBudget(ctx)
	if err != nil {
		return err
	}
	floor := windowStart(now)
	kept := map[string]int{}
	for hour, n := range rec.Hours {
		if t, err := time.Parse(hourLayout, hour); err == nil && !t.Before(floor) {
			kept[hour] = n
		}
	}
	kept[now.UTC().Format(hourLayout)]++
	return b.St.PutIdentifyBudget(ctx, store.IdentifyBudget{Hours: kept})
}

func windowStart(now time.Time) time.Time {
	return now.UTC().Add(-24 * time.Hour).Truncate(time.Hour)
}

func countSince(rec store.IdentifyBudget, now time.Time) int {
	floor := windowStart(now)
	total := 0
	for hour, n := range rec.Hours {
		if t, err := time.Parse(hourLayout, hour); err == nil && !t.Before(floor) {
			total += n
		}
	}
	return total
}
