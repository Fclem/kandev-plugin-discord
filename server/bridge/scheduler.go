package bridge

import (
	"context"
	"sync"
	"time"
)

// Scheduler runs jobs per key, one at a time and in submission order, on a
// goroutine that exists only while the key has work. It serializes all Host
// state writes for one task.
type Scheduler struct {
	ctx context.Context

	mu     sync.Mutex
	queues map[string][]func(context.Context)
	wg     sync.WaitGroup
}

// NewScheduler returns a scheduler whose jobs receive ctx.
func NewScheduler(ctx context.Context) *Scheduler {
	return &Scheduler{ctx: ctx, queues: map[string][]func(context.Context){}}
}

// Do queues job under key.
func (s *Scheduler) Do(key string, job func(context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, running := s.queues[key]
	s.queues[key] = append(q, job)
	if running {
		return
	}
	s.wg.Add(1)
	go s.drain(key)
}

func (s *Scheduler) drain(key string) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		q := s.queues[key]
		if len(q) == 0 {
			delete(s.queues, key)
			s.mu.Unlock()
			return
		}
		job := q[0]
		s.queues[key] = q[1:]
		s.mu.Unlock()
		if s.ctx.Err() == nil {
			job(s.ctx)
		}
	}
}

// Wait blocks until every queued job finished (tests).
func (s *Scheduler) Wait() { s.wg.Wait() }

// timers holds one pending delayed action per key; a later request with an
// earlier deadline replaces it.
type timers struct {
	mu  sync.Mutex
	set map[string]timerEntry
}

type timerEntry struct {
	t  *time.Timer
	at time.Time
}

func (t *timers) after(key string, d time.Duration, fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.set == nil {
		t.set = map[string]timerEntry{}
	}
	at := time.Now().Add(d)
	if e, ok := t.set[key]; ok {
		if !e.at.After(at) {
			return
		}
		e.t.Stop()
	}
	t.set[key] = timerEntry{at: at, t: time.AfterFunc(d, func() {
		t.mu.Lock()
		delete(t.set, key)
		t.mu.Unlock()
		fn()
	})}
}

func (t *timers) stopAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.set {
		e.t.Stop()
		delete(t.set, k)
	}
}

// lru remembers recent keys for deduplication.
type lru struct {
	mu    sync.Mutex
	max   int
	order []string
	seen  map[string]bool
}

func newLRU(max int) *lru { return &lru{max: max, seen: map[string]bool{}} }

// add reports whether key is new, and remembers it.
func (l *lru) add(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if key == "" || l.seen[key] {
		return key == ""
	}
	l.seen[key] = true
	l.order = append(l.order, key)
	if len(l.order) > l.max {
		delete(l.seen, l.order[0])
		l.order = l.order[1:]
	}
	return true
}
