package chaos

import (
	"context"
	"sort"
	"sync"
)

// sched models vLLM's priority scheduling (--scheduling-policy priority): at
// most `slots` requests run (max_num_seqs); the others wait in a queue ordered
// by (priority, arrival), a lower priority value first; a waiting request never
// preempts a running one (vLLM v1 scheduler: a running request is only
// preempted for KV-cache pressure, which the fake does not model).
type sched struct {
	mu      sync.Mutex
	slots   int
	running int
	seq     int64
	q       []*waiter
}

type waiter struct {
	prio int
	seq  int64
	ch   chan struct{}
}

func newSched(slots int) *sched { return &sched{slots: slots} }

// acquire blocks until the request may run; false when ctx ended first.
func (s *sched) acquire(ctx context.Context, prio int) bool {
	s.mu.Lock()
	if s.running < s.slots && len(s.q) == 0 {
		s.running++
		s.mu.Unlock()
		return true
	}
	s.seq++
	w := &waiter{prio: prio, seq: s.seq, ch: make(chan struct{})}
	s.q = append(s.q, w)
	sort.SliceStable(s.q, func(i, j int) bool {
		if s.q[i].prio != s.q[j].prio {
			return s.q[i].prio < s.q[j].prio
		}
		return s.q[i].seq < s.q[j].seq
	})
	s.mu.Unlock()
	select {
	case <-w.ch:
		return true
	case <-ctx.Done():
		s.mu.Lock()
		for i, x := range s.q {
			if x == w {
				s.q = append(s.q[:i], s.q[i+1:]...)
				s.mu.Unlock()
				return false
			}
		}
		s.mu.Unlock()
		<-w.ch // granted while giving up: take it and give it back
		s.release()
		return false
	}
}

func (s *sched) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.q) > 0 {
		w := s.q[0]
		s.q = s.q[1:]
		close(w.ch) // the slot passes to the waiter: running is unchanged
		return
	}
	s.running--
}

// waiting is the queue length (vllm:num_requests_waiting).
func (s *sched) waiting() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.q) }
