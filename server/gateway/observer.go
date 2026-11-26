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

// Debug and log lines of discordgo v0.29.0 the observer relies on; the
// observer tests pin them against the real library.
const (
	// identifyFormat precedes every IDENTIFY discordgo sends, including
	// invalid-session (op 9) re-identifies.
	identifyFormat = "Identify Packet: "
	// reidentifyFormat precedes the op 9 re-identify, on the goroutine
	// that then calls identify(), before the packet is built.
	reidentifyFormat = "sending identify packet to gateway in response to Op9"
	// reidentifyFailedFormat follows a re-identify that returned an error.
	reidentifyFailedFormat = "error sending gateway identify packet, %s, %s"
)

var (
	observerMu sync.Mutex
	observer   *Supervisor
	observed   *discordgo.Session
	// blocked holds the session's shard fields while an op 9 re-identify
	// is being refused (see refuseReidentify).
	blocked *[2]int
)

// Observe routes discordgo's package logger to sup: it charges every
// IDENTIFY to the budget, refuses an op 9 re-identify the budget has no
// room for, and records websocket close codes that discordgo otherwise
// swallows after the handshake. The session must log at discordgo.LogDebug
// for IDENTIFY to be visible. Arguments are never printed verbatim: the
// IDENTIFY dump carries the token and event dumps carry message content.
func Observe(sess *discordgo.Session, sup *Supervisor) {
	sup.init()
	sess.LogLevel = discordgo.LogDebug
	observerMu.Lock()
	observer, observed, blocked = sup, sess, nil
	observerMu.Unlock()
	discordgo.Logger = observe
}

func observe(level, _ int, format string, args ...any) {
	observerMu.Lock()
	sup, sess := observer, observed
	observerMu.Unlock()
	if sup == nil {
		return
	}
	switch {
	case strings.HasPrefix(format, identifyFormat):
		sup.identifySent()
		return
	case format == reidentifyFormat:
		if !sup.reserveIdentify() {
			refuseReidentify(sess)
		}
		return
	case format == reidentifyFailedFormat && endRefusal(sess):
		sup.refuse()
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

// refuseReidentify makes discordgo's next identify() fail before it writes
// anything: identify() rejects a shard ID outside the shard count. It runs
// on the goroutine about to call identify(), so nothing else reads the
// fields until endRefusal restores them on that same goroutine.
func refuseReidentify(sess *discordgo.Session) {
	observerMu.Lock()
	defer observerMu.Unlock()
	blocked = &[2]int{sess.ShardID, sess.ShardCount}
	sess.ShardID, sess.ShardCount = 2, 2
}

// endRefusal restores the shard fields after a refused re-identify and
// reports whether one was refused.
func endRefusal(sess *discordgo.Session) bool {
	observerMu.Lock()
	defer observerMu.Unlock()
	if blocked == nil {
		return false
	}
	sess.ShardID, sess.ShardCount = blocked[0], blocked[1]
	blocked = nil
	return true
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
