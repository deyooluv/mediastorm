package metadata

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPriorityLimiterGrantsIdleCallerImmediately(t *testing.T) {
	var l priorityLimiter
	started := time.Now()
	if err := l.Wait(context.Background(), time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("idle Wait took %v, want immediate", elapsed)
	}
}

func TestPriorityLimiterServesInteractiveBeforeQueuedBackground(t *testing.T) {
	var l priorityLimiter
	const interval = 20 * time.Millisecond
	if err := l.Wait(context.Background(), interval); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	var (
		mu    sync.Mutex
		order []string
		wg    sync.WaitGroup
	)
	record := func(name string, ctx context.Context) {
		defer wg.Done()
		if err := l.Wait(ctx, interval); err != nil {
			t.Errorf("%s Wait: %v", name, err)
			return
		}
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	// Queue a backlog of background work first, then an interactive caller.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go record("background", context.Background())
	}
	time.Sleep(5 * time.Millisecond)
	wg.Add(1)
	go record("interactive", WithInteractivePriority(context.Background()))
	wg.Wait()

	if len(order) != 6 {
		t.Fatalf("granted %d callers, want 6", len(order))
	}
	if order[0] != "interactive" {
		t.Fatalf("grant order = %v, want interactive first despite queued background callers", order)
	}
}

func TestPriorityLimiterWaitHonorsContextDeadline(t *testing.T) {
	var l priorityLimiter
	if err := l.Wait(context.Background(), time.Hour); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := l.Wait(ctx, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Wait returned after %v, want shortly after the 30ms deadline", elapsed)
	}

	l.mu.Lock()
	queued := len(l.interactive) + len(l.background)
	l.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d waiters still queued after cancellation, want 0", queued)
	}
}

func TestPriorityLimiterCooldownDelaysGrants(t *testing.T) {
	var l priorityLimiter
	l.BeginCooldown(50 * time.Millisecond)
	started := time.Now()
	if err := l.Wait(WithInteractivePriority(context.Background()), 0); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("Wait during cooldown took %v, want at least 40ms", elapsed)
	}
}
