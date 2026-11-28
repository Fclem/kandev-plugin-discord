package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

// Session is one gateway connection over a discordgo session. discordgo
// reads HELLO and READY without a deadline while holding its session lock,
// so Abort closes the TCP connection underneath to unblock Open, and makes
// any later dial of this session fail (an abort during gateway discovery
// would otherwise find no connection to close).
type Session struct {
	*discordgo.Session
	mu      sync.Mutex
	raw     net.Conn
	aborted bool
}

var _ Aborter = (*Session)(nil)

var errAborted = errors.New("connection attempt aborted")

// NewSession wraps s and routes its websocket dials through the session.
func NewSession(s *discordgo.Session) *Session {
	g := &Session{Session: s}
	s.Dialer = &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: HandshakeTimeout,
		NetDialContext:   g.dial,
	}
	return g
}

func (g *Session) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g.mu.Lock()
	aborted := g.aborted
	g.mu.Unlock()
	if aborted {
		return nil, errAborted
	}
	c, err := (&net.Dialer{Timeout: HandshakeTimeout, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.aborted { // aborted while dialing
		_ = c.Close()
		return nil, errAborted
	}
	g.raw = c
	return c, nil
}

// Abort closes the transport of the current connection, if any, and fails
// every later dial.
func (g *Session) Abort() {
	g.mu.Lock()
	g.aborted = true
	c := g.raw
	g.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}
