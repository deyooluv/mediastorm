// Package sourcehealth annotates indexer search results with server-side
// availability state so clients do not have to fan out per-result health and
// cache checks after a search.
//
// Debrid results get a safe quick cache check (no torrent add/remove) that
// runs in the background with a bounded overall deadline. The search waits a
// short inline budget for it; whatever has not finished is returned as
// "pending" with a token the client polls via Snapshot. Usenet results are
// never checked automatically (that would fetch NZBs and spend indexer grab
// quota); they are annotated from the outcome of recent explicit
// /usenet/health checks instead.
package sourcehealth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"strings"
	"sync"
	"time"

	"novastream/models"
	"novastream/services/debrid"
)

// DebridBulkChecker performs quick, non-mutating cache checks for many results.
type DebridBulkChecker interface {
	CheckQuickCacheOnlyBulk(ctx context.Context, candidates []models.NZBResult) ([]*debrid.DebridHealthCheck, error)
}

// Config tunes latency and memory bounds.
type Config struct {
	// InlineBudget is how long Annotate waits for the debrid check before
	// returning pending annotations.
	InlineBudget time.Duration
	// CheckTimeout bounds the background debrid check.
	CheckTimeout time.Duration
	// MaxDebridChecks caps how many debrid results (in rank order) are checked.
	MaxDebridChecks int
	// SessionTTL is how long a pending token can be polled.
	SessionTTL time.Duration
	// DebridResultTTL / UsenetResultTTL bound how long remembered states are reused.
	DebridResultTTL time.Duration
	UsenetResultTTL time.Duration
	// MaxCachedResults bounds the remembered-result cache.
	MaxCachedResults int
}

// DefaultConfig mirrors the previous client behaviour (top 50 debrid results,
// 60s request timeout) while keeping the search response fast.
func DefaultConfig() Config {
	return Config{
		InlineBudget:     1500 * time.Millisecond,
		CheckTimeout:     60 * time.Second,
		MaxDebridChecks:  50,
		SessionTTL:       5 * time.Minute,
		DebridResultTTL:  5 * time.Minute,
		UsenetResultTTL:  30 * time.Minute,
		MaxCachedResults: 5000,
	}
}

// IndexedHealth is the annotation for the result at Index in the original
// search response.
type IndexedHealth struct {
	Index  int                 `json:"index"`
	Health models.SourceHealth `json:"health"`
}

// Snapshot is the poll response for a token.
type Snapshot struct {
	Token    string          `json:"token"`
	Complete bool            `json:"complete"`
	Results  []IndexedHealth `json:"results"`
}

type cachedHealth struct {
	health    models.SourceHealth
	expiresAt time.Time
}

type session struct {
	token     string
	expiresAt time.Time
	done      chan struct{}
	indexes   []int
	// final is written once before done is closed.
	final map[int]models.SourceHealth
}

// Service is safe for concurrent use.
type Service struct {
	debrid DebridBulkChecker
	cfg    Config
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	cache    map[string]cachedHealth
}

// New creates a Service. debridChecker may be nil (debrid results are then
// annotated "unknown").
func New(debridChecker DebridBulkChecker, cfg Config) *Service {
	def := DefaultConfig()
	if cfg.InlineBudget < 0 {
		cfg.InlineBudget = 0
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = def.CheckTimeout
	}
	if cfg.MaxDebridChecks <= 0 {
		cfg.MaxDebridChecks = def.MaxDebridChecks
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = def.SessionTTL
	}
	if cfg.DebridResultTTL <= 0 {
		cfg.DebridResultTTL = def.DebridResultTTL
	}
	if cfg.UsenetResultTTL <= 0 {
		cfg.UsenetResultTTL = def.UsenetResultTTL
	}
	if cfg.MaxCachedResults <= 0 {
		cfg.MaxCachedResults = def.MaxCachedResults
	}
	return &Service{
		debrid:   debridChecker,
		cfg:      cfg,
		now:      time.Now,
		sessions: make(map[string]*session),
		cache:    make(map[string]cachedHealth),
	}
}

func isDebrid(r models.NZBResult) bool {
	return strings.EqualFold(string(r.ServiceType), string(models.ServiceTypeDebrid))
}

func isUsenet(r models.NZBResult) bool {
	st := strings.TrimSpace(string(r.ServiceType))
	return st == "" || strings.EqualFold(st, string(models.ServiceTypeUsenet))
}

func resultIdentity(r models.NZBResult) string {
	for _, v := range []string{r.GUID, r.DownloadURL, r.Link} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func usenetCacheKey(r models.NZBResult) string {
	id := resultIdentity(r)
	if id == "" {
		return ""
	}
	profile := ""
	if r.Attributes != nil {
		profile = strings.TrimSpace(r.Attributes["profileId"])
	}
	return "usenet|" + profile + "|" + id
}

func debridCacheKey(r models.NZBResult) string {
	key := debrid.QuickCacheKey(r)
	if key == "" {
		return ""
	}
	return "debrid|" + key
}

func (s *Service) cachedLocked(key string, now time.Time) (models.SourceHealth, bool) {
	if key == "" {
		return models.SourceHealth{}, false
	}
	entry, ok := s.cache[key]
	if !ok {
		return models.SourceHealth{}, false
	}
	if now.After(entry.expiresAt) {
		delete(s.cache, key)
		return models.SourceHealth{}, false
	}
	return entry.health, true
}

func (s *Service) storeLocked(key string, health models.SourceHealth, ttl time.Duration, now time.Time) {
	if key == "" {
		return
	}
	if len(s.cache) >= s.cfg.MaxCachedResults {
		for k, v := range s.cache {
			if now.After(v.expiresAt) {
				delete(s.cache, k)
			}
		}
		// Still full: drop arbitrary entries to stay bounded.
		for k := range s.cache {
			if len(s.cache) < s.cfg.MaxCachedResults {
				break
			}
			delete(s.cache, k)
		}
	}
	s.cache[key] = cachedHealth{health: health, expiresAt: now.Add(ttl)}
}

// RecordUsenetHealth remembers the outcome of an explicit usenet health check
// so later searches can annotate the same release for the same profile.
func (s *Service) RecordUsenetHealth(result models.NZBResult, check *models.NZBHealthCheck) {
	if s == nil || check == nil {
		return
	}
	now := s.now()
	state := models.SourceHealthUnhealthy
	if check.Healthy {
		state = models.SourceHealthHealthy
	}
	checkedAt := now
	health := models.SourceHealth{State: state, CheckedAt: &checkedAt, Status: check.Status}
	s.mu.Lock()
	s.storeLocked(usenetCacheKey(result), health, s.cfg.UsenetResultTTL, now)
	s.mu.Unlock()
}

func debridHealthToSource(check *debrid.DebridHealthCheck, checkedAt time.Time) models.SourceHealth {
	ts := checkedAt
	if check == nil {
		return models.SourceHealth{State: models.SourceHealthUnknown, CheckedAt: &ts, Error: "quick cache check did not run"}
	}
	h := models.SourceHealth{CheckedAt: &ts, Provider: check.Provider, Status: check.Status, Error: check.ErrorMessage}
	switch {
	case check.Status == "skipped" || check.Status == "error":
		h.State = models.SourceHealthUnknown
	case check.Cached:
		h.State = models.SourceHealthCached
	default:
		h.State = models.SourceHealthNotCached
	}
	return h
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Annotate sets SourceHealth on every usenet/debrid result in place. It waits
// at most InlineBudget (or until ctx is done) for debrid cache checks and
// returns the poll token when some annotations are still pending ("" when
// everything is final).
func (s *Service) Annotate(ctx context.Context, results []models.ScoredNZBResult) string {
	if s == nil || len(results) == 0 {
		return ""
	}
	now := s.now()
	var candidates []models.NZBResult
	var candidateIdx []int

	s.mu.Lock()
	for i := range results {
		r := results[i].NZBResult
		switch {
		case isDebrid(r):
			if h, ok := s.cachedLocked(debridCacheKey(r), now); ok {
				results[i].SourceHealth = &h
				continue
			}
			if s.debrid != nil && len(candidates) < s.cfg.MaxDebridChecks {
				candidates = append(candidates, r)
				candidateIdx = append(candidateIdx, i)
				continue
			}
			results[i].SourceHealth = &models.SourceHealth{State: models.SourceHealthUnknown}
		case isUsenet(r):
			if h, ok := s.cachedLocked(usenetCacheKey(r), now); ok {
				results[i].SourceHealth = &h
				continue
			}
			results[i].SourceHealth = &models.SourceHealth{State: models.SourceHealthUnknown}
		}
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		return ""
	}

	sess := &session{
		token:     newToken(),
		expiresAt: now.Add(s.cfg.SessionTTL),
		done:      make(chan struct{}),
		indexes:   candidateIdx,
	}
	s.registerSession(sess, now)
	go s.runDebridCheck(sess, candidates)

	timer := time.NewTimer(s.cfg.InlineBudget)
	defer timer.Stop()
	select {
	case <-sess.done:
		for _, idx := range candidateIdx {
			h := sess.final[idx]
			results[idx].SourceHealth = &h
		}
		return ""
	case <-timer.C:
	case <-ctx.Done():
	}
	for _, idx := range candidateIdx {
		results[idx].SourceHealth = &models.SourceHealth{State: models.SourceHealthPending, Token: sess.token}
	}
	return sess.token
}

func (s *Service) registerSession(sess *session, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, existing := range s.sessions {
		if now.After(existing.expiresAt) {
			delete(s.sessions, token)
		}
	}
	if sess.token != "" {
		s.sessions[sess.token] = sess
	}
}

func (s *Service) runDebridCheck(sess *session, candidates []models.NZBResult) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CheckTimeout)
	defer cancel()
	started := time.Now()
	checks, err := s.debrid.CheckQuickCacheOnlyBulk(ctx, candidates)
	finishedAt := s.now()

	final := make(map[int]models.SourceHealth, len(candidates))
	cached, notCached, unknown := 0, 0, 0
	s.mu.Lock()
	for i, idx := range sess.indexes {
		var check *debrid.DebridHealthCheck
		if i < len(checks) {
			check = checks[i]
		}
		h := debridHealthToSource(check, finishedAt)
		if err != nil && check == nil {
			h.Error = err.Error()
		}
		switch h.State {
		case models.SourceHealthCached:
			cached++
		case models.SourceHealthNotCached:
			notCached++
		default:
			unknown++
		}
		// Only remember definitive answers; transient errors retry next search.
		if check != nil && check.Status != "error" {
			s.storeLocked(debridCacheKey(candidates[i]), h, s.cfg.DebridResultTTL, finishedAt)
		}
		final[idx] = h
	}
	sess.final = final
	s.mu.Unlock()
	close(sess.done)

	log.Printf("[sourcehealth] debrid quick check count=%d cached=%d notCached=%d unknown=%d elapsed=%s err=%v",
		len(candidates), cached, notCached, unknown, time.Since(started).Round(time.Millisecond), err)
}

// Snapshot returns the annotations for token, waiting up to wait for the
// check to finish. ok is false when the token is unknown or expired.
func (s *Service) Snapshot(ctx context.Context, token string, wait time.Duration) (Snapshot, bool) {
	if s == nil {
		return Snapshot{}, false
	}
	token = strings.TrimSpace(token)
	s.mu.Lock()
	sess, ok := s.sessions[token]
	if ok && s.now().After(sess.expiresAt) {
		delete(s.sessions, token)
		ok = false
	}
	s.mu.Unlock()
	if !ok || token == "" {
		return Snapshot{}, false
	}

	complete := false
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-sess.done:
			complete = true
		case <-timer.C:
		case <-ctx.Done():
		}
	} else {
		select {
		case <-sess.done:
			complete = true
		default:
		}
	}

	out := Snapshot{Token: token, Complete: complete, Results: make([]IndexedHealth, 0, len(sess.indexes))}
	for _, idx := range sess.indexes {
		h := models.SourceHealth{State: models.SourceHealthPending, Token: token}
		if complete {
			h = sess.final[idx]
		}
		out.Results = append(out.Results, IndexedHealth{Index: idx, Health: h})
	}
	return out, true
}
