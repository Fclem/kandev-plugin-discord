package gateway

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

// identifyFormat is the debug line discordgo v0.29.0 writes immediately
// before every IDENTIFY it sends, including invalid-session (op 9)
// re-identifies. The observer test pins it against the real library.
const identifyFormat = "Identify Packet: "

var (
	observerMu sync.Mutex
	observer   *Supervisor
)

// Observe routes discordgo's package logger to sup: it charges every
// IDENTIFY to the budget and records websocket close codes that discordgo
// otherwise swallows after the handshake. The session must log at
// discordgo.LogDebug for IDENTIFY to be visible. Arguments are never
// printed verbatim: the IDENTIFY dump carries the token and event dumps
// carry message content.
func Observe(sess *discordgo.Session, sup *Supervisor) {
	sup.init()
	sess.LogLevel = discordgo.LogDebug
	observerMu.Lock()
	observer = sup
	observerMu.Unlock()
	discordgo.Logger = observe
}

func observe(level, _ int, format string, args ...any) {
	observerMu.Lock()
	sup := observer
	observerMu.Unlock()
	if sup == nil {
		return
	}
	if strings.HasPrefix(format, identifyFormat) {
		sup.identifySent()
		return
	}
	if level > discordgo.LogWarning {
		return
	}
	safe := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case error:
			var ce *websocket.CloseError
			if errors.As(v, &ce) {
				sup.ObserveClose(ce.Code)
			}
			safe[i] = redact(v.Error())
		case int, int64, string:
			safe[i] = v
		default:
			safe[i] = "…"
		}
	}
	if level == discordgo.LogError {
		log.Printf("discordgo: %s", redact(fmt.Sprintf(format, safe...)))
	}
}

// redact removes anything after a URL's host; gateway and REST URLs carry
// no token, but interaction callback paths do.
func redact(s string) string {
	for _, prefix := range []string{"https://", "wss://"} {
		for {
			i := strings.Index(s, prefix)
			if i < 0 {
				break
			}
			rest := s[i+len(prefix):]
			end := strings.IndexAny(rest, "/ \"'")
			host := rest
			tail := ""
			if end >= 0 {
				host, tail = rest[:end], rest[end:]
				if tail[0] == '/' {
					if j := strings.IndexAny(tail, " \"'"); j >= 0 {
						tail = tail[j:]
					} else {
						tail = ""
					}
				}
			}
			s = s[:i] + "<" + host + ">" + tail
		}
	}
	return s
}
