package metadata

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"novastream/models"
)

func backdropRankingFixture(t *testing.T, candidates int) ([]tmdbImageItem, *models.Image) {
	t.Helper()
	items := make([]tmdbImageItem, 0, candidates+1)
	for i := 0; i <= candidates; i++ {
		items = append(items, tmdbImageItem{FilePath: fmt.Sprintf("/backdrop-%02d.png", i), VoteAverage: float64(10 - i)})
	}
	primary := buildTMDBImage(items[0].FilePath, tmdbBackdropSize, "backdrop")
	return items, primary
}

func solidPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 18))
	for y := 0; y < 18; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 14), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestRankAlternateBackdropsFetchesSignaturesConcurrently(t *testing.T) {
	body := solidPNG(t)
	var inFlight, maxInFlight, calls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		n := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if n <= prev || maxInFlight.CompareAndSwap(prev, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(100 * time.Millisecond)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	c := newTMDBClient("test-key", "en", &http.Client{Transport: transport}, nil)
	items, primary := backdropRankingFixture(t, maxTMDBBackdropCandidates)

	started := time.Now()
	ranked := c.rankAlternateBackdrops(t.Context(), items, primary, nil)
	elapsed := time.Since(started)

	if len(ranked) != maxTMDBAlternateBackdrops {
		t.Fatalf("ranked %d backdrops, want %d", len(ranked), maxTMDBAlternateBackdrops)
	}
	if got := calls.Load(); got != int32(maxTMDBBackdropCandidates+1) {
		t.Fatalf("signature fetches = %d, want %d (primary + candidates)", got, maxTMDBBackdropCandidates+1)
	}
	// 11 fetches of 100ms each: serial ~1.1s, 6-way concurrent ~200ms.
	if elapsed > 600*time.Millisecond {
		t.Fatalf("ranking took %v, want concurrent signature fetches", elapsed)
	}
	if got := maxInFlight.Load(); got > tmdbBackdropSignatureConcurrency {
		t.Fatalf("max concurrent fetches = %d, want at most %d", got, tmdbBackdropSignatureConcurrency)
	}
}

func TestRankAlternateBackdropsFallsBackToVotesWhenBudgetExpires(t *testing.T) {
	previousBudget := tmdbBackdropSignatureBudget
	tmdbBackdropSignatureBudget = 50 * time.Millisecond
	t.Cleanup(func() { tmdbBackdropSignatureBudget = previousBudget })

	var once sync.Once
	release := make(chan struct{})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
			return nil, fmt.Errorf("released")
		}
	})
	c := newTMDBClient("test-key", "en", &http.Client{Transport: transport}, nil)
	items, primary := backdropRankingFixture(t, maxTMDBBackdropCandidates)

	started := time.Now()
	ranked := c.rankAlternateBackdrops(t.Context(), items, primary, nil)
	elapsed := time.Since(started)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("ranking took %v with a hung image host, want it bounded by the 50ms budget", elapsed)
	}
	if len(ranked) != maxTMDBAlternateBackdrops {
		t.Fatalf("ranked %d backdrops, want %d", len(ranked), maxTMDBAlternateBackdrops)
	}
	// Without signatures, candidates keep their vote order (primary excluded).
	for i, img := range ranked {
		want := buildTMDBImage(items[i+1].FilePath, tmdbBackdropSize, "backdrop").URL
		if img.URL != want {
			t.Fatalf("ranked[%d] = %s, want vote-ordered %s", i, img.URL, want)
		}
	}
}

func TestSeriesDetailsWithSeasonsFetchesSeasonsConcurrently(t *testing.T) {
	const seasonCount = 12
	var inFlight, maxInFlight atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		path := req.URL.Path
		body := `{}`
		switch {
		case strings.Contains(path, "/season/"):
			n := inFlight.Add(1)
			for {
				prev := maxInFlight.Load()
				if n <= prev || maxInFlight.CompareAndSwap(prev, n) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			inFlight.Add(-1)
			number := path[strings.LastIndex(path, "/")+1:]
			body = fmt.Sprintf(`{"id":%s,"name":"Season %s","season_number":%s,"episodes":[]}`, number, number, number)
		case strings.HasSuffix(path, "/tv/42"):
			var seasons []string
			for i := 1; i <= seasonCount; i++ {
				seasons = append(seasons, fmt.Sprintf(`{"id":%d,"name":"Season %d","season_number":%d,"episode_count":1}`, i, i, i))
			}
			body = `{"id":42,"name":"Long Show","seasons":[` + strings.Join(seasons, ",") + `]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	c := newTMDBClient("test-key", "en", &http.Client{Transport: transport}, nil)
	c.minInterval = 0

	started := time.Now()
	details, err := c.seriesDetailsWithSeasons(t.Context(), 42)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("seriesDetailsWithSeasons: %v", err)
	}
	if len(details.Seasons) != seasonCount {
		t.Fatalf("seasons = %d, want %d", len(details.Seasons), seasonCount)
	}
	for i, season := range details.Seasons {
		if season.Number != i+1 {
			t.Fatalf("season[%d].Number = %d, want ordered seasons", i, season.Number)
		}
	}
	// 12 seasons x 60ms: serial ~720ms, 6-way concurrent ~120ms.
	if elapsed > 450*time.Millisecond {
		t.Fatalf("seriesDetailsWithSeasons took %v, want concurrent season fetches", elapsed)
	}
	if got := maxInFlight.Load(); got > tmdbSeasonFetchConcurrency {
		t.Fatalf("max concurrent season fetches = %d, want at most %d", got, tmdbSeasonFetchConcurrency)
	}
}
