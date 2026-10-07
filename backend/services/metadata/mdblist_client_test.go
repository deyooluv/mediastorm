package metadata

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMDBListGetAllRatingsSharesConcurrentLookups(t *testing.T) {
	var calls atomic.Int32
	c := newMDBListClient("test-key", []string{"imdb"}, true, 24)
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ratings":[{"source":"imdb","value":7.5}]}`)),
			Header:     make(http.Header),
		}, nil
	})}

	var wg sync.WaitGroup
	results := make([]int, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ratings, err := c.GetAllRatings(context.Background(), "tt0111161", "movie")
			if err != nil {
				t.Errorf("GetAllRatings: %v", err)
				return
			}
			results[i] = len(ratings)
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("API calls = %d, want 1 shared call for concurrent lookups of one title", got)
	}
	for i, n := range results {
		if n != 1 {
			t.Fatalf("caller %d got %d ratings, want 1", i, n)
		}
	}
}

func TestMDBListGetAllRatingsHonorsCallerDeadlineWhileQueued(t *testing.T) {
	c := newMDBListClient("test-key", []string{"imdb"}, true, 24)
	c.minInterval = time.Hour
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ratings":[]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	// The first lookup takes the only slot for the next hour.
	if _, err := c.GetAllRatings(context.Background(), "tt0000001", "movie"); err != nil {
		t.Fatalf("first GetAllRatings: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.GetAllRatings(ctx, "tt0000002", "movie")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetAllRatings error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("queued lookup returned after %v, want it bounded by the 40ms deadline", elapsed)
	}
}
