package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/store"
)

type fakeConn struct {
	mu     sync.Mutex
	opens  int
	errs   []error
	closes int
	opened chan struct{}
}

func (c *fakeConn) Open() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens++
	var err error
	if len(c.errs) > 0 {
		err, c.errs = c.errs[0], c.errs[1:]
	}
	if err == nil && c.opened != nil {
		c.opened <- struct{}{}
	}
	return err
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}

func instant(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func newSupervisor(conn Conn, budget Budget) (*Supervisor, *[]State) {
	var mu sync.Mutex
	states := &[]State{}
	return &Supervisor{
		Dial: func() Conn { return conn }, Budget: budget, After: instant,
		Report: func(s State) { mu.Lock(); *states = append(*states, s); mu.Unlock() },
	}, states
}

func budget() *StoreBudget { return &StoreBudget{St: store.New(store.NewMemoryHost())} }

func TestFatalCloseCodesStopReconnecting(t *testing.T) {
	for _, code := range []int{4004, 4013, 4014} {
		conn := &fakeConn{errs: []error{errors.New("dial"), &websocket.CloseError{Code: code, Text: "nope"}}}
		s, _ := newSupervisor(conn, budget())
		final := s.Run(context.Background())
		assert.Equal(t, StateFatal, final.Name)
		assert.Equal(t, code, final.CloseCode)
		assert.NotEmpty(t, final.Reason)
		assert.Equal(t, 2, conn.opens, "retries transient errors, stops on %d", code)
	}
}

func TestRestHaltStopsBeforeConnecting(t *testing.T) {
	conn := &fakeConn{}
	s, _ := newSupervisor(conn, budget())
	s.Halted = func() bool { return true }
	final := s.Run(context.Background())
	assert.Equal(t, StateHalted, final.Name)
	assert.Equal(t, 0, conn.opens)
}

func TestIdentifyBudgetRefusesToConnect(t *testing.T) {
	ctx := context.Background()
	b := budget()
	now := time.Date(2026, 11, 25, 12, 30, 0, 0, time.UTC)
	for i := range IdentifyLimit {
		require.NoError(t, b.Record(ctx, now.Add(-time.Duration(i%20)*time.Hour)))
	}
	conn := &fakeConn{}
	s, states := newSupervisor(conn, b)
	s.Now = func() time.Time { return now }
	runCtx, cancel := context.WithCancel(ctx)
	s.After = func(time.Duration) <-chan time.Time { cancel(); return instant(0) }
	s.Run(runCtx)
	assert.Equal(t, 0, conn.opens)
	assert.Equal(t, StateRefused, (*states)[0].Name)
	assert.Equal(t, IdentifyLimit, (*states)[0].Identifies24h)
}

func TestBudgetWindowForgetsOldIdentifies(t *testing.T) {
	ctx := context.Background()
	b := budget()
	now := time.Date(2026, 11, 25, 12, 30, 0, 0, time.UTC)
	require.NoError(t, b.Record(ctx, now.Add(-26*time.Hour)))
	require.NoError(t, b.Record(ctx, now.Add(-2*time.Hour)))
	require.NoError(t, b.Record(ctx, now))
	n, err := b.Count(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestReconnectsAfterDisconnect(t *testing.T) {
	conn := &fakeConn{opened: make(chan struct{}, 4)}
	s, _ := newSupervisor(conn, budget())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan State)
	go func() { done <- s.Run(ctx) }()
	<-conn.opened
	s.Disconnected()
	<-conn.opened
	cancel()
	final := <-done
	assert.Equal(t, StateStopped, final.Name)
	assert.Equal(t, 2, conn.opens)
	assert.Equal(t, 2, conn.closes, "each connection is closed: after the drop and at the stop")
}

func TestRestHaltWhileConnectedClosesTheSocket(t *testing.T) {
	conn := &fakeConn{opened: make(chan struct{}, 2)}
	s, _ := newSupervisor(conn, budget())
	var halted atomic.Bool
	s.Halted = halted.Load
	done := make(chan State)
	go func() { done <- s.Run(context.Background()) }()
	<-conn.opened
	halted.Store(true)
	s.Halt()
	final := <-done
	assert.Equal(t, StateHalted, final.Name)
	assert.Equal(t, 1, conn.opens)
	assert.Equal(t, 1, conn.closes)
}

func TestBackoffIsBounded(t *testing.T) {
	s := &Supervisor{MinBackoff: time.Second, MaxBackoff: 8 * time.Second}
	s.init()
	for attempt := range 10 {
		d := s.backoff(attempt)
		assert.GreaterOrEqual(t, d, 750*time.Millisecond)
		assert.LessOrEqual(t, d, 10*time.Second)
	}
}
