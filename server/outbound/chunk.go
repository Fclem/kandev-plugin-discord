// Package outbound mirrors Kandev task activity into the task channel:
// conversation messages after a cursor, session status lines, and the pinned
// task card.
package outbound

import (
	"strings"
	"unicode/utf8"
)

// Limits for one rendered Kandev message.
const (
	MaxMessageLength = 2000
	MaxChunks        = 5
	closeFence       = "\n```"
)

// Chunk splits text into Discord messages of at most limit bytes (bytes ≥
// characters, so the 2000-character limit holds). Splits fall on line
// boundaries when possible; a code fence open at a split is closed and
// reopened with the same info string in the next chunk.
func Chunk(text string, limit int) []string {
	c := chunker{limit: limit}
	for _, line := range strings.SplitAfter(text, "\n") {
		c.add(line)
	}
	c.flush()
	return c.out
}

type chunker struct {
	limit int
	out   []string
	cur   strings.Builder
	fence string // opening fence line while inside a code block
	// header is the reopened fence at the start of cur (not content).
	header int
}

func isFence(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "```") }

func (c *chunker) reserve(fence string) int {
	if fence != "" {
		return len(closeFence)
	}
	return 0
}

func (c *chunker) add(line string) {
	after := c.fence
	if isFence(line) {
		if c.fence == "" {
			after = strings.TrimSpace(line)
		} else {
			after = ""
		}
	}
	need := len(line) + c.reserve(after)
	if c.cur.Len()+need > c.limit && c.cur.Len() > c.header {
		c.flush()
	}
	for c.cur.Len()+len(line)+c.reserve(c.fence) > c.limit {
		room := c.limit - c.cur.Len() - c.reserve(c.fence)
		cut := runeBoundary(line, max(room, 1))
		c.cur.WriteString(line[:cut])
		line = line[cut:]
		c.flush()
	}
	c.cur.WriteString(line)
	c.fence = after
}

func (c *chunker) flush() {
	s := c.cur.String()
	if strings.TrimSpace(s[c.header:]) != "" {
		if c.fence != "" {
			s = strings.TrimSuffix(s, "\n") + closeFence
		}
		c.out = append(c.out, strings.TrimRight(s, "\n"))
	}
	c.cur.Reset()
	c.header = 0
	if c.fence != "" {
		c.cur.WriteString(c.fence + "\n")
		c.header = c.cur.Len()
	}
}

// runeBoundary returns the largest index ≤ n that does not split a rune.
func runeBoundary(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		_, size := utf8.DecodeRuneInString(s)
		return size
	}
	return n
}

// Limit caps rendered chunks at MaxChunks and reports truncation.
func Limit(chunks []string) ([]string, bool) {
	if len(chunks) <= MaxChunks {
		return chunks, false
	}
	return chunks[:MaxChunks], true
}
