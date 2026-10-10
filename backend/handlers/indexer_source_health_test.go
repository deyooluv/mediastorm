package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"novastream/models"
	"novastream/services/debrid"
	"novastream/services/sourcehealth"
)

type gatedDebridChecker struct {
	release chan struct{}
}

func (g *gatedDebridChecker) CheckQuickCacheOnlyBulk(ctx context.Context, candidates []models.NZBResult) ([]*debrid.DebridHealthCheck, error) {
	if g.release != nil {
		<-g.release
	}
	out := make([]*debrid.DebridHealthCheck, len(candidates))
	for i := range candidates {
		out[i] = &debrid.DebridHealthCheck{Status: "cached", Cached: true, Healthy: true, Provider: "torbox"}
	}
	return out, nil
}

func sourceHealthSearchFixture() *fakeIndexerService {
	return &fakeIndexerService{results: []models.NZBResult{
		{Title: "Usenet", GUID: "u1", ServiceType: models.ServiceTypeUsenet},
		{Title: "Torrent", GUID: "t1", ServiceType: models.ServiceTypeDebrid, Attributes: map[string]string{"infoHash": "abc"}},
	}}
}

func TestIndexerSearchSourceHealthOptIn(t *testing.T) {
	handler := NewIndexerHandler(sourceHealthSearchFixture(), false)
	handler.SetSourceHealthService(sourcehealth.New(&gatedDebridChecker{}, sourcehealth.DefaultConfig()))

	// Without the opt-in flag the response is unchanged.
	rec := httptest.NewRecorder()
	handler.Search(rec, httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=x&includeFiltered=true", nil))
	var plain []models.ScoredNZBResult
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range plain {
		if r.SourceHealth != nil {
			t.Fatalf("unexpected annotation without opt-in: %+v", r.SourceHealth)
		}
	}

	rec = httptest.NewRecorder()
	handler.Search(rec, httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=x&includeFiltered=true&includeSourceHealth=true", nil))
	var annotated []models.ScoredNZBResult
	if err := json.Unmarshal(rec.Body.Bytes(), &annotated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if annotated[0].SourceHealth == nil || annotated[0].SourceHealth.State != models.SourceHealthUnknown {
		t.Fatalf("usenet: expected unknown, got %+v", annotated[0].SourceHealth)
	}
	if annotated[1].SourceHealth == nil || annotated[1].SourceHealth.State != models.SourceHealthCached {
		t.Fatalf("debrid: expected cached, got %+v", annotated[1].SourceHealth)
	}
}

func TestIndexerSearchSourceHealthPendingAndPoll(t *testing.T) {
	gate := &gatedDebridChecker{release: make(chan struct{})}
	cfg := sourcehealth.DefaultConfig()
	cfg.InlineBudget = 10 * time.Millisecond
	handler := NewIndexerHandler(sourceHealthSearchFixture(), false)
	handler.SetSourceHealthService(sourcehealth.New(gate, cfg))

	rec := httptest.NewRecorder()
	handler.Search(rec, httptest.NewRequest(http.MethodGet,
		"/api/indexers/search?q=x&includeFiltered=true&includeAdaptiveSummary=true&includeSourceHealth=true", nil))
	var body struct {
		Results           []models.ScoredNZBResult `json:"results"`
		SourceHealthToken string                   `json:"sourceHealthToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SourceHealthToken == "" {
		t.Fatal("expected sourceHealthToken in wrapped response")
	}
	pending := body.Results[1].SourceHealth
	if pending == nil || pending.State != models.SourceHealthPending || pending.Token != body.SourceHealthToken {
		t.Fatalf("expected pending annotation carrying token, got %+v", pending)
	}

	close(gate.release)
	rec = httptest.NewRecorder()
	handler.SourceHealth(rec, httptest.NewRequest(http.MethodGet,
		"/api/indexers/source-health?token="+body.SourceHealthToken+"&waitMs=2000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status %d: %s", rec.Code, rec.Body.String())
	}
	var snap sourcehealth.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if !snap.Complete || len(snap.Results) != 1 || snap.Results[0].Index != 1 || snap.Results[0].Health.State != models.SourceHealthCached {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	rec = httptest.NewRecorder()
	handler.SourceHealth(rec, httptest.NewRequest(http.MethodGet, "/api/indexers/source-health?token=missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown token, got %d", rec.Code)
	}
}

type recordingUsenetService struct{}

func (recordingUsenetService) CheckHealth(context.Context, models.NZBResult) (*models.NZBHealthCheck, error) {
	return &models.NZBHealthCheck{Healthy: true, Status: "healthy"}, nil
}

func TestUsenetHealthCheckFeedsSearchAnnotation(t *testing.T) {
	svc := sourcehealth.New(nil, sourcehealth.DefaultConfig())
	usenet := NewUsenetHandler(recordingUsenetService{})
	usenet.SetHealthRecorder(svc)

	rec := httptest.NewRecorder()
	usenet.CheckHealth(rec, httptest.NewRequest(http.MethodPost, "/api/usenet/health",
		strings.NewReader(`{"result":{"title":"Usenet","guid":"u1","serviceType":"usenet"},"profileId":"p1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("health status %d", rec.Code)
	}

	handler := NewIndexerHandler(sourceHealthSearchFixture(), false)
	handler.SetSourceHealthService(svc)
	rec = httptest.NewRecorder()
	handler.Search(rec, httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=x&includeFiltered=true&includeSourceHealth=true&userId=p1", nil))
	var annotated []models.ScoredNZBResult
	if err := json.Unmarshal(rec.Body.Bytes(), &annotated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h := annotated[0].SourceHealth; h == nil || h.State != models.SourceHealthHealthy || h.CheckedAt == nil {
		t.Fatalf("expected remembered healthy usenet annotation, got %+v", h)
	}
}

func TestIndexerSearchSourceHealthPlainResults(t *testing.T) {
	handler := NewIndexerHandler(sourceHealthSearchFixture(), false)
	handler.SetSourceHealthService(sourcehealth.New(&gatedDebridChecker{}, sourcehealth.DefaultConfig()))
	rec := httptest.NewRecorder()
	handler.Search(rec, httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=x&includeSourceHealth=true", nil))
	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("expected 2 results, got %d", len(raw))
	}
	if _, scored := raw[0]["filterStatus"]; scored {
		t.Fatal("plain search must not gain scoring fields")
	}
	health, _ := raw[1]["sourceHealth"].(map[string]any)
	if health["state"] != models.SourceHealthCached {
		t.Fatalf("expected cached annotation on plain result, got %v", raw[1]["sourceHealth"])
	}
}
