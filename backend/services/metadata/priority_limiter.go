package metadata

import (
	"context"
	"sync"
	"time"
)

type requestPriorityKey struct{}

// WithInteractivePriority marks upstream metadata calls made for a person who
// is waiting on screen (e.g. a details page). Shared provider rate limiters
// grant these calls before queued background enrichment.
func WithInteractivePriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestPriorityKey{}, true)
}

// IsInteractivePriority reports whether ctx was marked by WithInteractivePriority.
func IsInteractivePriority(ctx context.Context) bool {
	interactive, _ := ctx.Value(requestPriorityKey{}).(bool)
	return interactive
}

// priorityLimiter spaces provider requests by a minimum interval. Unlike a
// FIFO slot reservation, queued interactive waiters are always granted before
// queued background waiters, so a burst of background work cannot push a
// details request seconds into the future. Waiting honors ctx cancellation.
// The zero value is ready to use.
type priorityLimiter struct {
	mu            sync.Mutex
	next          time.Time // earliest time the next request may be granted
	cooldownUntil time.Time
	interactive   []*limiterWaiter
	background    []*limiterWaiter
	timer         *time.Timer
}

type limiterWaiter struct {
	ready    chan struct{}
	interval time.Duration
	granted  bool
}

// Wait blocks until the caller may issue one request, then reserves the
// following interval for it.
func (l *priorityLimiter) Wait(ctx context.Context, interval time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	l.mu.Lock()
	now := time.Now()
	if len(l.interactive) == 0 && len(l.background) == 0 && !now.Before(l.availableAtLocked()) {
		l.next = now.Add(interval)
		l.mu.Unlock()
		return nil
	}
	w := &limiterWaiter{ready: make(chan struct{}), interval: interval}
	if IsInteractivePriority(ctx) {
		l.interactive = append(l.interactive, w)
	} else {
		l.background = append(l.background, w)
	}
	l.scheduleLocked(now)
	l.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		if !w.granted {
			l.interactive = removeLimiterWaiter(l.interactive, w)
			l.background = removeLimiterWaiter(l.background, w)
		}
		l.mu.Unlock()
		return ctx.Err()
	}
}

// BeginCooldown blocks all grants for delay, e.g. after a provider 429.
func (l *priorityLimiter) BeginCooldown(delay time.Duration) {
	if delay <= 0 {
		return
	}
	until := time.Now().Add(delay)
	l.mu.Lock()
	if until.After(l.cooldownUntil) {
		l.cooldownUntil = until
	}
	l.mu.Unlock()
}

func (l *priorityLimiter) availableAtLocked() time.Time {
	if l.cooldownUntil.After(l.next) {
		return l.cooldownUntil
	}
	return l.next
}

func (l *priorityLimiter) scheduleLocked(now time.Time) {
	if l.timer != nil {
		return
	}
	delay := l.availableAtLocked().Sub(now)
	if delay < 0 {
		delay = 0
	}
	l.timer = time.AfterFunc(delay, l.dispatch)
}

func (l *priorityLimiter) dispatch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timer = nil

	now := time.Now()
	if now.Before(l.availableAtLocked()) {
		// A cooldown began after this dispatch was scheduled.
		if len(l.interactive)+len(l.background) > 0 {
			l.scheduleLocked(now)
		}
		return
	}

	var w *limiterWaiter
	switch {
	case len(l.interactive) > 0:
		w, l.interactive = l.interactive[0], l.interactive[1:]
	case len(l.background) > 0:
		w, l.background = l.background[0], l.background[1:]
	default:
		return
	}
	w.granted = true
	close(w.ready)
	l.next = now.Add(w.interval)
	if len(l.interactive)+len(l.background) > 0 {
		l.scheduleLocked(now)
	}
}

func removeLimiterWaiter(waiters []*limiterWaiter, target *limiterWaiter) []*limiterWaiter {
	for i, w := range waiters {
		if w == target {
			return append(waiters[:i], waiters[i+1:]...)
		}
	}
	return waiters
}
