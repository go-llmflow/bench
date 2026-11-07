package chaos

import (
	"context"
	"testing"
	"time"
)

func TestSchedOrdersWaitersByPriorityThenArrivalAndNeverPreempts(t *testing.T) {
	s := newSched(1)
	ctx := context.Background()
	if !s.acquire(ctx, 100) { // a low-priority request is running
		t.Fatal("first must run")
	}
	order := make(chan int, 4)
	for i, p := range []int{100, 30, 0, 30} {
		i, p := i, p
		go func() { s.acquire(ctx, p); order <- i }()
		for s.waiting() != i+1 { // arrivals in a fixed order
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-order:
		t.Fatal("a waiting request preempted the running one")
	case <-time.After(30 * time.Millisecond):
	}
	var got []int
	for k := 0; k < 4; k++ {
		s.release()
		got = append(got, <-order)
	}
	// priorities 0 (#2), then 30 by arrival (#1, #3), then 100 (#0)
	want := []int{2, 1, 3, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestSchedGivingUpFreesTheQueue(t *testing.T) {
	s := newSched(1)
	s.acquire(context.Background(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- s.acquire(ctx, 5) }()
	for s.waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if <-done || s.waiting() != 0 {
		t.Fatal("a cancelled waiter must leave the queue")
	}
}
