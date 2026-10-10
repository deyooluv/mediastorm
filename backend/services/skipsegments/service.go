package skipsegments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	DefaultIntroDBURL = "https://api.introdb.app/segments"
	DefaultSkipDBURL  = "https://api.skipdb.tv/api/segments"

	// Upstream lookups must never hold up playback: each provider gets a short
	// budget and failures are negatively cached briefly.
	defaultProviderTimeout = 4 * time.Second
	foundTTL               = 6 * time.Hour
	emptyTTL               = 30 * time.Minute
	errorTTL               = time.Minute
	maxCacheEntries        = 4096
	maxResponseBytes       = 128 * 1024
)

var imdbIDPattern = regexp.MustCompile(`^tt[0-9]+$`)

// ErrInvalidEpisode is returned when the provider lookup key is incomplete.
var ErrInvalidEpisode = errors.New("valid imdbId, season, and episode are required")

// Options configures a Service. Zero values use production defaults.
type Options struct {
	IntroDBURL      string
	SkipDBURL       string
	ProviderTimeout time.Duration
	HTTPClient      *http.Client
}

// Service fetches and caches IntroDB/SkipDB lookups.
type Service struct {
	introDBURL string
	skipDBURL  string
	timeout    time.Duration
	client     *http.Client
	flights    singleflight.Group

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	value     any
	err       error
	expiresAt time.Time
}

// New builds a Service.
func New(options Options) *Service {
	service := &Service{
		introDBURL: options.IntroDBURL,
		skipDBURL:  options.SkipDBURL,
		timeout:    options.ProviderTimeout,
		client:     options.HTTPClient,
		cache:      make(map[string]cacheEntry),
	}
	if service.introDBURL == "" {
		service.introDBURL = DefaultIntroDBURL
	}
	if service.skipDBURL == "" {
		service.skipDBURL = DefaultSkipDBURL
	}
	if service.timeout <= 0 {
		service.timeout = defaultProviderTimeout
	}
	if service.client == nil {
		service.client = &http.Client{}
	}
	return service
}

// EpisodeKey identifies a provider lookup.
type EpisodeKey struct {
	IMDbID  string
	Season  int
	Episode int
}

// NormalizeEpisodeKey lowercases/trims the IMDb ID and validates the key.
func NormalizeEpisodeKey(imdbID string, season, episode int) (EpisodeKey, error) {
	key := EpisodeKey{IMDbID: strings.ToLower(strings.TrimSpace(imdbID)), Season: season, Episode: episode}
	if !imdbIDPattern.MatchString(key.IMDbID) || season <= 0 || episode <= 0 {
		return EpisodeKey{}, ErrInvalidEpisode
	}
	return key, nil
}

// LookupResult holds the raw provider responses. A nil response with a nil error
// means the provider had no data (or, for SkipDB, was not needed).
type LookupResult struct {
	IntroDB    *IntroDBResponse
	SkipDB     *SkipDBResponse
	IntroDBErr error
	SkipDBErr  error
}

// Lookup queries IntroDB, then SkipDB only when IntroDB lacks a valid intro, recap
// or outro for this duration (seconds, 0 = unknown). Both lookups are cached.
func (s *Service) Lookup(ctx context.Context, key EpisodeKey, duration float64) LookupResult {
	duration = SanitizeDuration(duration)
	var result LookupResult
	result.IntroDB, result.IntroDBErr = s.introDB(ctx, key)
	if IntroDBComplete(result.IntroDB, duration) {
		return result
	}
	result.SkipDB, result.SkipDBErr = s.skipDB(ctx, key, math.Round(duration))
	return result
}

func (s *Service) introDB(ctx context.Context, key EpisodeKey) (*IntroDBResponse, error) {
	query := url.Values{"imdb_id": {key.IMDbID}, "season": {strconv.Itoa(key.Season)}, "episode": {strconv.Itoa(key.Episode)}}
	cacheKey := fmt.Sprintf("introdb:%s:%d:%d", key.IMDbID, key.Season, key.Episode)
	value, err := s.cached(ctx, cacheKey, func(fetchCtx context.Context) (any, bool, error) {
		var response IntroDBResponse
		found, err := s.fetchJSON(fetchCtx, s.introDBURL+"?"+query.Encode(), &response)
		if err != nil || !found {
			return (*IntroDBResponse)(nil), false, err
		}
		hasData := response.Intro != nil || response.Recap != nil || response.Outro != nil
		return &response, hasData, nil
	})
	response, _ := value.(*IntroDBResponse)
	return response, err
}

func (s *Service) skipDB(ctx context.Context, key EpisodeKey, roundedDuration float64) (*SkipDBResponse, error) {
	query := url.Values{"imdb_id": {key.IMDbID}, "season": {strconv.Itoa(key.Season)}, "episode": {strconv.Itoa(key.Episode)}}
	if roundedDuration > 0 {
		query.Set("duration", strconv.FormatFloat(roundedDuration, 'f', 0, 64))
	}
	cacheKey := fmt.Sprintf("skipdb:%s:%d:%d:%.0f", key.IMDbID, key.Season, key.Episode, roundedDuration)
	value, err := s.cached(ctx, cacheKey, func(fetchCtx context.Context) (any, bool, error) {
		var response SkipDBResponse
		found, err := s.fetchJSON(fetchCtx, s.skipDBURL+"?"+query.Encode(), &response)
		if err != nil || !found {
			return (*SkipDBResponse)(nil), false, err
		}
		segments := response.Segments
		hasData := segments.Intro != nil || segments.Recap != nil || segments.Outro != nil || segments.Preview != nil
		return &response, hasData, nil
	})
	response, _ := value.(*SkipDBResponse)
	return response, err
}

// cached returns a cached provider value or performs one shared fetch. The fetch
// runs detached from the caller's context (bounded by the provider timeout) so a
// cancelled request cannot poison concurrent callers, while each caller still
// stops waiting when its own context ends.
func (s *Service) cached(ctx context.Context, key string, fetch func(context.Context) (any, bool, error)) (any, error) {
	if entry, ok := s.getCache(key); ok {
		return entry.value, entry.err
	}
	channel := s.flights.DoChan(key, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		value, hasData, err := fetch(fetchCtx)
		ttl := emptyTTL
		switch {
		case err != nil:
			ttl = errorTTL
		case hasData:
			ttl = foundTTL
		}
		s.setCache(key, cacheEntry{value: value, err: err, expiresAt: time.Now().Add(ttl)})
		return cacheEntry{value: value, err: err}, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-channel:
		entry := result.Val.(cacheEntry)
		return entry.value, entry.err
	}
}

func (s *Service) getCache(key string) (cacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return cacheEntry{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(s.cache, key)
		return cacheEntry{}, false
	}
	return entry, true
}

func (s *Service) setCache(key string, entry cacheEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCacheEntries {
		now := time.Now()
		for existing, value := range s.cache {
			if now.After(value.expiresAt) {
				delete(s.cache, existing)
			}
		}
		if len(s.cache) >= maxCacheEntries {
			s.cache = make(map[string]cacheEntry)
		}
	}
	s.cache[key] = entry
}

// fetchJSON returns found=false for a 404 (provider has no record).
func (s *Service) fetchJSON(ctx context.Context, endpoint string, destination any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MediaStorm/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("segment provider returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(destination); err != nil {
		return false, err
	}
	return true, nil
}
