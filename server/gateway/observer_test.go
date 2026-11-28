package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kandev-plugin-discord/server/store"
)

// fakeGateway is a loopback Discord gateway driving the real discordgo
// client: it answers REST /gateway, then for each connection sends Hello,
// reads IDENTIFY, and runs script.
type fakeGateway struct {
	srv        *httptest.Server
	dials      atomic.Int32
	identifies atomic.Int32
	script     func(c *websocket.Conn, dial int32)
}

func newFakeGateway(t *testing.T, script func(c *websocket.Conn, dial int32)) *fakeGateway {
	t.Helper()
	g := &fakeGateway{script: script}
	up := websocket.Upgrader{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gateway") {
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + strings.TrimPrefix(g.srv.URL, "http://") + "/ws"})
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		dial := g.dials.Add(1)
		_ = c.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 45000}})
		g.script(c, dial)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// readIdentify reads frames until an IDENTIFY (op 2) and counts it.
func (g *fakeGateway) readIdentify(c *websocket.Conn) bool {
	for {
		var frame struct {
			Op int `json:"op"`
		}
		if err := c.ReadJSON(&frame); err != nil {
			return false
		}
		if frame.Op == 2 {
			g.identifies.Add(1)
			return true
		}
	}
}

func ready(c *websocket.Conn, seq int) {
	_ = c.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": seq, "d": map[string]any{
		"session_id": "sess", "user": map[string]any{"id": "1", "username": "bot"}, "guilds": []any{},
	}})
}

type rewrite struct{ target *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func session(t *testing.T, g *fakeGateway) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	target, _ := url.Parse(g.srv.URL)
	s.Client = &http.Client{Transport: rewrite{target}}
	s.ShouldReconnectOnError = false
	s.StateEnabled = false
	return s
}

func runSupervised(t *testing.T, s *discordgo.Session, budget Budget) (State, *Supervisor) {
	t.Helper()
	sup := &Supervisor{Dial: func() Conn { return s }, Budget: budget, After: instant}
	Observe(s, sup)
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { sup.Disconnected() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return sup.Run(ctx), sup
}

func TestFatalCloseAfterReadyStopsWithoutRedialing(t *testing.T) {
	var g *fakeGateway
	g = newFakeGateway(t, func(c *websocket.Conn, _ int32) {
		if !g.readIdentify(c) {
			return
		}
		ready(c, 1)
		time.Sleep(50 * time.Millisecond)
		_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4014, "Disallowed intent(s)."))
		time.Sleep(100 * time.Millisecond)
	})
	final, _ := runSupervised(t, session(t, g), budget())
	assert.Equal(t, StateFatal, final.Name)
	assert.Equal(t, 4014, final.CloseCode)
	assert.Equal(t, int32(1), g.dials.Load(), "no second connection after a fatal close")
}

func TestEveryIdentifyIsChargedIncludingInvalidSession(t *testing.T) {
	b := budget()
	var g *fakeGateway
	g = newFakeGateway(t, func(c *websocket.Conn, _ int32) {
		if !g.readIdentify(c) {
			return
		}
		_ = c.WriteJSON(map[string]any{"op": 9, "d": false}) // invalid session: discordgo re-identifies in place
		if !g.readIdentify(c) {
			return
		}
		ready(c, 1)
		time.Sleep(50 * time.Millisecond)
		_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "Authentication failed."))
		time.Sleep(100 * time.Millisecond)
	})
	final, _ := runSupervised(t, session(t, g), b)
	assert.Equal(t, StateFatal, final.Name)
	assert.Equal(t, 4004, final.CloseCode)
	assert.Equal(t, int32(2), g.identifies.Load())
	n, err := b.Count(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both IDENTIFY packets charged")
}

func TestReidentifyBeyondTheBudgetIsNeverSent(t *testing.T) {
	b := budget()
	for range IdentifyLimit - 1 {
		require.NoError(t, b.Record(context.Background(), time.Now()))
	}
	var g *fakeGateway
	extra := make(chan bool, 1)
	g = newFakeGateway(t, func(c *websocket.Conn, _ int32) {
		if !g.readIdentify(c) { // the 900th: within the budget
			return
		}
		_ = c.WriteJSON(map[string]any{"op": 9, "d": false})
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		extra <- g.readIdentify(c) // the client must close instead of re-identifying
	})
	s := session(t, g)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var refused atomic.Bool
	sup := &Supervisor{Dial: func() Conn { return s }, Budget: b, After: instant, Report: func(st State) {
		if st.Name == StateRefused {
			refused.Store(true)
			cancel()
		}
	}}
	Observe(s, sup)
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { sup.Disconnected() })
	sup.Run(ctx)

	assert.True(t, refused.Load(), "the gateway waits for budget room")
	assert.False(t, <-extra, "no IDENTIFY beyond the budget")
	assert.Equal(t, int32(1), g.identifies.Load())
	assert.Equal(t, int32(1), g.dials.Load())
	n, err := b.Count(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Equal(t, IdentifyLimit, n)
	assert.Equal(t, 1, s.ShardCount, "the session's shard fields are restored")
	assert.Equal(t, 0, s.ShardID)
}

func TestRedactKeepsOnlyHosts(t *testing.T) {
	got := redact("Post \"https://discord.com/api/v9/interactions/1/secret-token/callback\": timeout")
	assert.NotContains(t, got, "secret-token")
	assert.Contains(t, got, "<discord.com>")
}

// freshSessions dials a new observed session per attempt, as the bridge does.
func freshSessions(t *testing.T, g *fakeGateway, sup *Supervisor) func() Conn {
	return func() Conn {
		s := session(t, g)
		Observe(s, sup)
		s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { sup.Disconnected() })
		return NewSession(s)
	}
}

func TestInvalidatedSessionIsNeverResumed(t *testing.T) {
	var g *fakeGateway
	second := make(chan int, 1)
	g = newFakeGateway(t, func(c *websocket.Conn, dial int32) {
		if dial == 1 {
			if !g.readIdentify(c) {
				return
			}
			ready(c, 1)
			time.Sleep(50 * time.Millisecond)
			_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4009, "Session timed out."))
			time.Sleep(100 * time.Millisecond)
			return
		}
		var frame struct {
			Op int `json:"op"`
		}
		_ = c.ReadJSON(&frame)
		second <- frame.Op
		_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "stop the test"))
		time.Sleep(100 * time.Millisecond)
	})
	sup := &Supervisor{Budget: budget(), After: instant}
	sup.Dial = freshSessions(t, g, sup)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sup.Run(ctx)
	assert.Equal(t, 2, <-second, "a new session IDENTIFYs (op 2) instead of RESUMEing (op 6)")
}

// silentGateway upgrades the websocket and then never speaks.
func silentGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	up := websocket.Upgrader{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gateway") {
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + strings.TrimPrefix(g.srv.URL, "http://") + "/ws"})
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		g.dials.Add(1)
		defer c.Close()
		_, _, _ = c.ReadMessage() // until the client gives up
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func TestStalledHandshakeTimesOutAndHaltInterruptsIt(t *testing.T) {
	g := silentGateway(t)
	var timedOut atomic.Bool
	var halted atomic.Bool
	sup := &Supervisor{Budget: budget(), After: instant, Handshake: 200 * time.Millisecond, Halted: halted.Load}
	sup.Report = func(st State) {
		if st.Name == StateReconnecting && strings.Contains(st.Reason, "timed out") && timedOut.CompareAndSwap(false, true) {
			sup.Handshake = time.Minute // the next attempt waits for the halt
			go func() {
				time.Sleep(100 * time.Millisecond)
				halted.Store(true)
				sup.Halt()
			}()
		}
	}
	sup.Dial = freshSessions(t, g, sup)
	done := make(chan State, 1)
	go func() { done <- sup.Run(context.Background()) }()
	select {
	case final := <-done:
		assert.True(t, timedOut.Load(), "the first attempt timed out")
		assert.Equal(t, StateHalted, final.Name, "the halt interrupted the second handshake")
		assert.GreaterOrEqual(t, g.dials.Load(), int32(2))
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor stayed blocked in the handshake")
	}
}

func TestUnwritableBudgetRefusesToIdentify(t *testing.T) {
	g := newFakeGateway(t, func(c *websocket.Conn, _ int32) {})
	mem := store.NewMemoryHost()
	b := &StoreBudget{St: store.New(readOnly{mem})}
	ctx, cancel := context.WithCancel(context.Background())
	var reports []State
	sup := &Supervisor{Budget: b, After: func(time.Duration) <-chan time.Time { cancel(); return instant(0) }}
	sup.Report = func(st State) { reports = append(reports, st) }
	sup.Dial = freshSessions(t, g, sup)
	sup.Run(ctx)
	assert.Equal(t, int32(0), g.dials.Load(), "no connection without a recorded reservation")
	require.NotEmpty(t, reports)
	assert.Contains(t, reports[0].Reason, "identify counter unavailable")
}

// readOnly is a state host whose writes fail.
type readOnly struct{ *store.MemoryHost }

func (readOnly) SetState(context.Context, string, string, string, map[string]any) error {
	return errors.New("state is read-only")
}

func TestReconnectBeforeReadyCannotWedgeTheSupervisor(t *testing.T) {
	var g *fakeGateway
	g = newFakeGateway(t, func(c *websocket.Conn, _ int32) {
		if !g.readIdentify(c) {
			return
		}
		_ = c.WriteJSON(map[string]any{"op": 7, "d": nil}) // discordgo deadlocks in Open on this
		time.Sleep(time.Second)
	})
	sup := &Supervisor{Budget: budget(), Handshake: 100 * time.Millisecond}
	sup.init()
	done := make(chan error, 1)
	go func() { done <- sup.open(context.Background(), NewSession(session(t, g))) }()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, errHandshakeTimeout)
	case <-time.After(2 * time.Second):
		t.Fatal("open stayed blocked after its handshake timeout")
	}
}

func TestAbortDuringGatewayDiscoveryPreventsTheDial(t *testing.T) {
	release := make(chan struct{})
	var upgrades atomic.Int32
	g := &fakeGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gateway") {
			<-release
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + strings.TrimPrefix(g.srv.URL, "http://") + "/ws"})
			return
		}
		upgrades.Add(1)
	}))
	t.Cleanup(g.srv.Close)
	t.Cleanup(func() { close(release) })
	s := NewSession(session(t, g))
	opened := make(chan error, 1)
	go func() { opened <- s.Open() }()
	time.Sleep(50 * time.Millisecond) // Open is waiting on GET /gateway
	s.Abort()
	release <- struct{}{}
	select {
	case err := <-opened:
		assert.ErrorIs(t, err, errAborted)
	case <-time.After(2 * time.Second):
		t.Fatal("Open dialed after the abort")
	}
	assert.Zero(t, upgrades.Load())
}

func TestTokenRejectedDuringGatewayDiscoveryHalts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":0,"message":"401: Unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	sup := &Supervisor{Budget: budget(), After: instant}
	sup.Dial = func() Conn {
		s, _ := discordgo.New("Bot test-token")
		s.Client = &http.Client{Transport: rewrite{target}}
		return NewSession(s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.Equal(t, StateHalted, sup.Run(ctx).Name)
}
