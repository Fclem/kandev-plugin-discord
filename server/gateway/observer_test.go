package gateway

import (
	"context"
	"encoding/json"
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
	sup := &Supervisor{Conn: s, Budget: budget, After: instant}
	Observe(s, sup)
	t.Cleanup(func() { discordgo.Logger = nil })
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

func TestRedactKeepsOnlyHosts(t *testing.T) {
	got := redact("Post \"https://discord.com/api/v9/interactions/1/secret-token/callback\": timeout")
	assert.NotContains(t, got, "secret-token")
	assert.Contains(t, got, "<discord.com>")
}
