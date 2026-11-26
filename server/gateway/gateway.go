// Package gateway supervises the Discord gateway websocket. discordgo's own
// reconnect loop is disabled: the supervisor opens a fresh session per
// attempt with jittered backoff, bounds the handshake, stops on fatal close
// codes, honors the REST 401 halt, and enforces the identify budget
// (Discord resets the token after 1000 identifies a day).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"kandev-plugin-discord/server/store"
)

// Intents the plugin needs; MESSAGE_CONTENT is privileged.
const Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

// IdentifyLimit is the plugin's cap per rolling 24 h, below Discord's 1000.
const IdentifyLimit = 900

// HandshakeTimeout bounds one connection attempt, from dial to READY.
const HandshakeTimeout = 30 * time.Second

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

// Conn is one gateway connection; *discordgo.Session fits.
type Conn interface {
	Open() error
	Close() error
}

// Aborter is a Conn whose blocked Open can be broken by closing its
// transport (discordgo reads the handshake without a deadline).
type Aborter interface {
	Abort()
}

// Budget counts identifies in a rolling 24 h window. A reservation is made
// before an IDENTIFY may be sent, so a counter that cannot be written
// refuses instead of letting the IDENTIFY through.
type Budget interface {
	Count(ctx context.Context, now time.Time) (int, error)
	// Reserve records one identify at now when fewer than limit were
	// recorded in the last 24 h. It returns the count before reserving.
	Reserve(ctx context.Context, now time.Time, limit int) (count int, ok bool, err error)
	// Release takes back a reservation made at `at` whose IDENTIFY was
	// never sent.
	Release(ctx context.Context, at time.Time) error
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
	// Dial returns the connection for one attempt. A fresh session per
	// attempt always IDENTIFYs and never RESUMEs a session Discord may have
	// invalidated (close 4007/4009); the bridge reconciles after READY.
	Dial   func() Conn
	Budget Budget
	// Halted reports a REST 401 (discordapi.Client.Halted).
	Halted func() bool
	// Report receives every state change.
	Report func(State)

	MinBackoff, MaxBackoff, BudgetRetry, Handshake time.Duration
	Now                                            func() time.Time
	After                                          func(time.Duration) <-chan time.Time

	once         sync.Once
	disconnected chan struct{}
	lastClose    atomic.Int64
	runCtx       atomic.Pointer[context.Context]
	// identified is set once the attempt's IDENTIFY was written.
	identified atomic.Bool
	// refused is set when an op 9 re-identify was refused for the budget.
	refused atomic.Bool
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
		if s.Handshake == 0 {
			s.Handshake = HandshakeTimeout
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

// Halt wakes the supervisor after a REST 401 so it closes the socket (or
// aborts a handshake) and stops instead of waiting for Discord to drop it.
func (s *Supervisor) Halt() { s.Disconnected() }

// ObserveClose records a websocket close code seen anywhere in the
// connection's life (the handshake or the read loop).
func (s *Supervisor) ObserveClose(code int) {
	s.init()
	s.lastClose.Store(int64(code))
}

// identifySent notes that discordgo wrote an IDENTIFY; its reservation was
// made before (Run for the first, reserveIdentify for an op 9 re-identify).
func (s *Supervisor) identifySent() {
	s.init()
	s.identified.Store(true)
}

func (s *Supervisor) ctx() context.Context {
	if p := s.runCtx.Load(); p != nil {
		return context.WithoutCancel(*p)
	}
	return context.Background()
}

// reserveIdentify reserves budget for an op 9 re-identify. A full budget or
// a counter that cannot be read or written refuses.
func (s *Supervisor) reserveIdentify() bool {
	s.init()
	_, ok, err := s.Budget.Reserve(s.ctx(), s.Now(), IdentifyLimit)
	return err == nil && ok
}

// refuse wakes Run after an op 9 re-identify was refused: the socket is
// open but unidentified, so Run closes it and waits for budget room.
func (s *Supervisor) refuse() {
	s.refused.Store(true)
	s.Disconnected()
}

// fatalClose returns the recorded fatal close code, clearing the record.
func (s *Supervisor) fatalClose() int {
	code := int(s.lastClose.Swap(0))
	if fatalReasons[code] == "" {
		return 0
	}
	return code
}

const haltReason = "Discord rejected the bot token (HTTP 401): reset the token and save the settings"

var errHandshakeTimeout = errors.New("the gateway handshake timed out")

// Run blocks until ctx ends or a fatal condition stops the gateway.
func (s *Supervisor) Run(ctx context.Context) State {
	s.init()
	s.runCtx.Store(&ctx)
	attempt := 0
	for {
		if ctx.Err() != nil {
			return s.stop(StateStopped, 0, "")
		}
		if s.Halted() {
			return s.stop(StateHalted, 0, haltReason)
		}
		now := s.Now()
		count, ok, err := s.Budget.Reserve(ctx, now, IdentifyLimit)
		if err != nil {
			s.Report(State{Name: StateReconnecting, Reason: "identify counter unavailable: " + err.Error()})
			s.sleep(ctx, s.backoff(attempt))
			attempt++
			continue
		}
		if !ok {
			s.Report(State{Name: StateRefused, Identifies24h: count,
				Reason: fmt.Sprintf("%d identifies in the last 24 h; waiting below %d", count, IdentifyLimit)})
			s.sleep(ctx, s.BudgetRetry)
			continue
		}
		s.drainDisconnect()
		s.lastClose.Store(0)
		s.identified.Store(false)
		s.Report(State{Name: StateConnecting, Identifies24h: count + 1})
		conn := s.Dial()
		err = s.open(ctx, conn)
		if !s.identified.Load() {
			// The attempt failed before its IDENTIFY: give the budget back.
			if err := s.Budget.Release(s.ctx(), now); err != nil {
				s.Report(State{Name: StateReconnecting, Reason: "identify counter not released: " + err.Error()})
			}
		}
		if err != nil {
			if s.refused.Swap(false) || s.Halted() || ctx.Err() != nil {
				continue // the loop reports the refusal, the halt, or the stop
			}
			code := closeCode(err)
			if c := s.fatalClose(); c != 0 {
				code = c
			}
			if fatalReasons[code] != "" {
				_ = conn.Close()
				return s.stop(StateFatal, code, fatalReasons[code])
			}
			s.Report(State{Name: StateReconnecting, Identifies24h: count + 1, Reason: openReason(err)})
			s.sleep(ctx, s.backoff(attempt))
			attempt++
			continue
		}
		attempt = 0
		s.Report(State{Name: StateConnected, Identifies24h: count + 1})
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return s.stop(StateStopped, 0, "")
		case <-s.disconnected:
			_ = conn.Close()
			if s.Halted() {
				return s.stop(StateHalted, 0, haltReason)
			}
			if s.refused.Swap(false) {
				continue // the budget check reports the refusal and waits
			}
			if code := s.fatalClose(); code != 0 {
				return s.stop(StateFatal, code, fatalReasons[code])
			}
			s.Report(State{Name: StateReconnecting, Reason: "gateway connection closed"})
			s.sleep(ctx, s.backoff(0))
		}
	}
}

// open runs conn.Open, bounded by the handshake timeout and interruptible
// by ctx or a halt, when conn can abort its transport.
func (s *Supervisor) open(ctx context.Context, conn Conn) error {
	ab, ok := conn.(Aborter)
	if !ok {
		return conn.Open()
	}
	done := make(chan error, 1)
	go func() { done <- conn.Open() }()
	timer := time.NewTimer(s.Handshake)
	defer timer.Stop()
	var cause error
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cause = ctx.Err()
	case <-s.disconnected:
		cause = errors.New("connection attempt interrupted")
	case <-timer.C:
		cause = errHandshakeTimeout
	}
	ab.Abort()
	if err := <-done; err == nil {
		_ = conn.Close() // completed as it was aborted
	}
	return cause
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

var _ Budget = (*StoreBudget)(nil)

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

// Reserve counts and records under one lock, so concurrent reservations
// never pass the limit together.
func (b *StoreBudget) Reserve(ctx context.Context, now time.Time, limit int) (int, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.St.IdentifyBudget(ctx)
	if err != nil {
		return 0, false, err
	}
	count := countSince(rec, now)
	if count >= limit {
		return count, false, nil
	}
	return count, true, b.add(ctx, rec, now, 1)
}

// Record adds one identify to the hour of now (tests seed with it).
func (b *StoreBudget) Record(ctx context.Context, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.St.IdentifyBudget(ctx)
	if err != nil {
		return err
	}
	return b.add(ctx, rec, now, 1)
}

// Release removes one identify from the hour of at.
func (b *StoreBudget) Release(ctx context.Context, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.St.IdentifyBudget(ctx)
	if err != nil {
		return err
	}
	return b.add(ctx, rec, at, -1)
}

// add changes the hour of at by delta and prunes buckets older than 24 h.
func (b *StoreBudget) add(ctx context.Context, rec store.IdentifyBudget, at time.Time, delta int) error {
	floor := windowStart(at)
	kept := map[string]int{}
	for hour, n := range rec.Hours {
		if t, err := time.Parse(hourLayout, hour); err == nil && !t.Before(floor) {
			kept[hour] = n
		}
	}
	hour := at.UTC().Format(hourLayout)
	if kept[hour] += delta; kept[hour] <= 0 {
		delete(kept, hour)
	}
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
