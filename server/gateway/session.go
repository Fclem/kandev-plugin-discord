package gateway

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

// Session is one gateway connection over a discordgo session. discordgo
// reads HELLO and READY without a deadline while holding its session lock,
// so Abort closes the TCP connection underneath to unblock Open.
type Session struct {
	*discordgo.Session
	mu  sync.Mutex
	raw net.Conn
}

var _ Aborter = (*Session)(nil)

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
	c, err := (&net.Dialer{Timeout: HandshakeTimeout, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.raw = c
	g.mu.Unlock()
	return c, nil
}

// Abort closes the transport of the current connection, if any.
func (g *Session) Abort() {
	g.mu.Lock()
	c := g.raw
	g.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}
