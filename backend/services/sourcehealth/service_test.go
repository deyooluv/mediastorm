package sourcehealth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"novastream/models"
	"novastream/services/debrid"
)

type fakeDebrid struct {
	mu      sync.Mutex
	calls   int
	gotLens []int
	release chan struct{}
	err     error
	cached  map[string]bool // infoHash -> cached
	skipped map[string]bool
}

func (f *fakeDebrid) CheckQuickCacheOnlyBulk(ctx context.Context, candidates []models.NZBResult) ([]*debrid.DebridHealthCheck, error) {
	f.mu.Lock()
	f.calls++
	f.gotLens = append(f.gotLens, len(candidates))
	f.mu.Unlock()
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return make([]*debrid.DebridHealthCheck, len(candidates)), ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*debrid.DebridHealthCheck, len(candidates))
	for i, c := range candidates {
		hash := c.Attributes["infoHash"]
		switch {
		case f.skipped[hash]:
			out[i] = &debrid.DebridHealthCheck{Status: "skipped"}
		case f.cached[hash]:
			out[i] = &debrid.DebridHealthCheck{Status: "cached", Cached: true, Healthy: true, Provider: "torbox"}
		default:
			out[i] = &debrid.DebridHealthCheck{Status: "not_cached", Provider: "torbox"}
		}
	}
	return out, nil
}

func (f *fakeDebrid) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func debridResult(hash string) models.ScoredNZBResult {
	return models.ScoredNZBResult{NZBResult: models.NZBResult{
		Title:       "debrid " + hash,
		GUID:        "guid-" + hash,
		ServiceType: models.ServiceTypeDebrid,
		Attributes:  map[string]string{"infoHash": hash},
	}}
}

func usenetResult(guid, profile string) models.ScoredNZBResult {
	return models.ScoredNZBResult{NZBResult: models.NZBResult{
		Title:       "usenet " + guid,
		GUID:        guid,
		ServiceType: models.ServiceTypeUsenet,
		Attributes:  map[string]string{"profileId": profile},
	}}
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.InlineBudget = 2 * time.Second
	cfg.CheckTimeout = 5 * time.Second
	return cfg
}

func TestAnnotateInlineCompletion(t *testing.T) {
	fake := &fakeDebrid{cached: map[string]bool{"aaa": true}, skipped: map[string]bool{"ccc": true}}
	svc := New(fake, testConfig())
	results := []models.ScoredNZBResult{
		debridResult("aaa"),
		debridResult("bbb"),
		debridResult("ccc"),
		usenetResult("u1", "p1"),
		{NZBResult: models.NZBResult{Title: "other", ServiceType: "peartube"}},
	}

	token := svc.Annotate(context.Background(), results)
	if token != "" {
		t.Fatalf("expected no token when checks finish inline, got %q", token)
	}
	want := []string{models.SourceHealthCached, models.SourceHealthNotCached, models.SourceHealthUnknown, models.SourceHealthUnknown}
	for i, state := range want {
		if results[i].SourceHealth == nil || results[i].SourceHealth.State != state {
			t.Fatalf("result %d: want %q, got %+v", i, state, results[i].SourceHealth)
		}
	}
	if results[0].SourceHealth.CheckedAt == nil || results[0].SourceHealth.Provider != "torbox" {
		t.Fatalf("expected checkedAt/provider on cached annotation: %+v", results[0].SourceHealth)
	}
	if results[4].SourceHealth != nil {
		t.Fatalf("non usenet/debrid results must not be annotated: %+v", results[4].SourceHealth)
	}

	// Second search reuses remembered debrid states without another provider call.
	again := []models.ScoredNZBResult{debridResult("aaa"), debridResult("bbb")}
	if tok := svc.Annotate(context.Background(), again); tok != "" {
		t.Fatalf("expected cached annotations, got token %q", tok)
	}
	if fake.callCount() != 1 {
		t.Fatalf("expected 1 provider call, got %d", fake.callCount())
	}
	if again[0].SourceHealth.State != models.SourceHealthCached || again[1].SourceHealth.State != models.SourceHealthNotCached {
		t.Fatalf("unexpected cached annotations: %+v %+v", again[0].SourceHealth, again[1].SourceHealth)
	}
}

func TestAnnotatePendingThenSnapshot(t *testing.T) {
	fake := &fakeDebrid{release: make(chan struct{}), cached: map[string]bool{"aaa": true}}
	cfg := testConfig()
	cfg.InlineBudget = 20 * time.Millisecond
	svc := New(fake, cfg)
	results := []models.ScoredNZBResult{usenetResult("u1", ""), debridResult("aaa"), debridResult("bbb")}

	token := svc.Annotate(context.Background(), results)
	if token == "" {
		t.Fatal("expected a poll token for a slow check")
	}
	for _, idx := range []int{1, 2} {
		h := results[idx].SourceHealth
		if h == nil || h.State != models.SourceHealthPending || h.Token != token {
			t.Fatalf("result %d: expected pending with token, got %+v", idx, h)
		}
	}

	snap, ok := svc.Snapshot(context.Background(), token, 0)
	if !ok || snap.Complete || len(snap.Results) != 2 || snap.Results[0].Health.State != models.SourceHealthPending {
		t.Fatalf("expected incomplete snapshot, got ok=%v %+v", ok, snap)
	}

	close(fake.release)
	snap, ok = svc.Snapshot(context.Background(), token, 2*time.Second)
	if !ok || !snap.Complete {
		t.Fatalf("expected complete snapshot, got ok=%v %+v", ok, snap)
	}
	got := map[int]string{}
	for _, item := range snap.Results {
		got[item.Index] = item.Health.State
	}
	if got[1] != models.SourceHealthCached || got[2] != models.SourceHealthNotCached {
		t.Fatalf("unexpected final states: %+v", got)
	}

	if _, ok := svc.Snapshot(context.Background(), "nope", 0); ok {
		t.Fatal("unknown token should not resolve")
	}
}

func TestAnnotateCapsDebridChecks(t *testing.T) {
	fake := &fakeDebrid{}
	cfg := testConfig()
	cfg.MaxDebridChecks = 2
	svc := New(fake, cfg)
	results := []models.ScoredNZBResult{debridResult("a"), debridResult("b"), debridResult("c")}
	svc.Annotate(context.Background(), results)
	if len(fake.gotLens) != 1 || fake.gotLens[0] != 2 {
		t.Fatalf("expected one bulk call of 2, got %v", fake.gotLens)
	}
	if results[2].SourceHealth == nil || results[2].SourceHealth.State != models.SourceHealthUnknown {
		t.Fatalf("over-cap result should be unknown, got %+v", results[2].SourceHealth)
	}
}

func TestAnnotateErrorsAreNotRemembered(t *testing.T) {
	fake := &fakeDebrid{err: errors.New("provider down")}
	svc := New(fake, testConfig())
	results := []models.ScoredNZBResult{debridResult("a")}
	svc.Annotate(context.Background(), results)
	if h := results[0].SourceHealth; h == nil || h.State != models.SourceHealthUnknown || h.Error == "" {
		t.Fatalf("expected unknown with error, got %+v", h)
	}
	fake.err = nil
	results = []models.ScoredNZBResult{debridResult("a")}
	svc.Annotate(context.Background(), results)
	if fake.callCount() != 2 {
		t.Fatalf("errors must not be cached; calls=%d", fake.callCount())
	}
}

func TestRecordUsenetHealthIsProfileScoped(t *testing.T) {
	svc := New(nil, testConfig())
	checked := usenetResult("u1", "p1")
	svc.RecordUsenetHealth(checked.NZBResult, &models.NZBHealthCheck{Healthy: true, Status: "healthy"})
	svc.RecordUsenetHealth(usenetResult("u2", "p1").NZBResult, &models.NZBHealthCheck{Healthy: false, Status: "incomplete"})

	results := []models.ScoredNZBResult{usenetResult("u1", "p1"), usenetResult("u1", "p2"), usenetResult("u2", "p1"), debridResult("x")}
	if tok := svc.Annotate(context.Background(), results); tok != "" {
		t.Fatalf("no debrid checker means no pending work, got token %q", tok)
	}
	want := []string{models.SourceHealthHealthy, models.SourceHealthUnknown, models.SourceHealthUnhealthy, models.SourceHealthUnknown}
	for i, state := range want {
		if results[i].SourceHealth == nil || results[i].SourceHealth.State != state {
			t.Fatalf("result %d: want %q, got %+v", i, state, results[i].SourceHealth)
		}
	}
}

func TestRememberedResultsExpire(t *testing.T) {
	svc := New(nil, testConfig())
	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	svc.RecordUsenetHealth(usenetResult("u1", "").NZBResult, &models.NZBHealthCheck{Healthy: true})
	now = now.Add(svc.cfg.UsenetResultTTL + time.Second)
	results := []models.ScoredNZBResult{usenetResult("u1", "")}
	svc.Annotate(context.Background(), results)
	if results[0].SourceHealth.State != models.SourceHealthUnknown {
		t.Fatalf("expected expired memory to be unknown, got %+v", results[0].SourceHealth)
	}
}
