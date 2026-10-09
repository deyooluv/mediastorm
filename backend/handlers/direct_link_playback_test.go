package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"novastream/models"
)

const directLinkTestPath = "/debrid/torbox/42/Movie.2024.2160p.mkv"

func directLinkHeartbeat(path string) models.PlaybackProgressUpdate {
	return models.PlaybackProgressUpdate{
		MediaType:  "movie",
		ItemID:     "tmdb:movie:42",
		MovieName:  "Movie",
		Position:   120,
		Duration:   6000,
		SourcePath: path,
	}
}

func TestDirectLinkHeartbeatWithoutIssuedURLIsIgnored(t *testing.T) {
	tracker := newTestTracker()
	// Ordinary app heartbeats for proxied playback must never create entries.
	if tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", directLinkHeartbeat(directLinkTestPath)) {
		t.Fatal("heartbeat without an issued direct URL was attributed")
	}
	if got := len(tracker.GetActiveDirectLinkPlaybacks()); got != 0 {
		t.Fatalf("active direct links = %d, want 0", got)
	}
	if got := tracker.CountPlaybackSlots(); got != 0 {
		t.Fatalf("slots = %d, want 0", got)
	}
}

func TestDirectLinkHeartbeatTracksAndCountsOnce(t *testing.T) {
	tracker := newTestTracker()
	tracker.RecordDirectURLIssued("/webdav"+directLinkTestPath, "acct1")

	for i := 0; i < 3; i++ {
		if !tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", directLinkHeartbeat(directLinkTestPath)) {
			t.Fatal("heartbeat for issued direct URL was not attributed")
		}
	}

	playbacks := tracker.GetActiveDirectLinkPlaybacks()
	if len(playbacks) != 1 {
		t.Fatalf("active direct links = %d, want 1", len(playbacks))
	}
	if playbacks[0].ClientID != "tv" || playbacks[0].Position != 120 || playbacks[0].MediaMetadata.Title != "Movie" {
		t.Fatalf("unexpected playback: %+v", playbacks[0])
	}
	if got := tracker.CountPlaybackSlots(); got != 1 {
		t.Fatalf("global slots = %d, want 1", got)
	}
	if got := tracker.CountForAccount("acct1"); got != 1 {
		t.Fatalf("account slots = %d, want 1", got)
	}
	if got := tracker.CountForProfile("p1"); got != 1 {
		t.Fatalf("profile slots = %d, want 1", got)
	}
	if got := tracker.CountForAccount("acct2"); got != 0 {
		t.Fatalf("other account slots = %d, want 0", got)
	}
}

func TestDirectLinkDedupesAgainstTrackedStream(t *testing.T) {
	tracker := newTestTracker()
	tracker.RecordDirectURLIssued(directLinkTestPath, "acct1")
	tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", directLinkHeartbeat(directLinkTestPath))

	// The client falls back to the proxy for the same source.
	req := httptest.NewRequest(http.MethodGet, "/video/stream?profileId=p1&mediaType=movie&itemId=tmdb:movie:42", nil)
	tracker.StartStreamWithAccount(req, directLinkTestPath, 1000, 0, 0, "acct1")

	if got := len(tracker.GetActiveDirectLinkPlaybacks()); got != 0 {
		t.Fatalf("direct link shown alongside tracked stream: %d", got)
	}
	if got := tracker.CountForAccount("acct1"); got != 1 {
		t.Fatalf("account slots = %d, want 1", got)
	}
	// Later heartbeats keep being owned by the tracked stream.
	if tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", directLinkHeartbeat(directLinkTestPath)) {
		t.Fatal("heartbeat attributed to direct link while backend serves it")
	}
}

func TestDirectLinkEndsOnFinalHeartbeatAndStaleness(t *testing.T) {
	tracker := newTestTracker()
	tracker.RecordDirectURLIssued(directLinkTestPath, "acct1")
	tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", directLinkHeartbeat(directLinkTestPath))

	ended := directLinkHeartbeat(directLinkTestPath)
	ended.PlaybackEnded = true
	tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", ended)
	if got := tracker.CountPlaybackSlots(); got != 0 {
		t.Fatalf("slots after final heartbeat = %d, want 0", got)
	}

	tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", directLinkHeartbeat(directLinkTestPath))
	tracker.mu.Lock()
	for _, playback := range tracker.directLinkPlaybacks {
		playback.LastHeartbeat = time.Now().Add(-directLinkHeartbeatTimeout - time.Second)
	}
	tracker.mu.Unlock()
	if got := tracker.CountPlaybackSlots(); got != 0 {
		t.Fatalf("slots after missed heartbeats = %d, want 0", got)
	}
	if got := len(tracker.GetActiveDirectLinkPlaybacks()); got != 0 {
		t.Fatalf("stale direct links = %d, want 0", got)
	}
}

func TestDirectLinkIgnoresOtherAccountsAndLive(t *testing.T) {
	tracker := newTestTracker()
	tracker.RecordDirectURLIssued(directLinkTestPath, "acct1")
	if tracker.ObserveDirectLinkHeartbeat("p9", "acct2", "tv", "10.0.0.9", directLinkHeartbeat(directLinkTestPath)) {
		t.Fatal("heartbeat from another account matched the grant")
	}

	tracker.RecordDirectURLIssued("live/channel.ts", "acct1")
	live := directLinkHeartbeat("live/channel.ts")
	live.MediaType = "live"
	if tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", "10.0.0.5", live) {
		t.Fatal("live heartbeat tracked as a VOD direct link")
	}
}

func TestDirectLinkConsumesLimitButAllowsSamePlaybackReRequest(t *testing.T) {
	tracker := newTestTracker()
	tracker.RecordDirectURLIssued(directLinkTestPath, "acct1")
	clientIP := getClientIP(httptest.NewRequest(http.MethodGet, "/", nil))
	tracker.ObserveDirectLinkHeartbeat("p1", "acct1", "tv", clientIP, directLinkHeartbeat(directLinkTestPath))

	// Re-resolving the same source (no profile params, same device) keeps its slot.
	reResolve := httptest.NewRequest(http.MethodGet, "/video/direct-url?path="+directLinkTestPath, nil)
	if _, exceeds := tracker.WouldExceedAccountLimit(reResolve, directLinkTestPath, "acct1", 1); exceeds {
		t.Fatal("re-requesting the active direct link was rejected")
	}

	// A different playback on the account is over the limit.
	other := httptest.NewRequest(http.MethodGet, "/video/stream?profileId=p2&mediaType=movie&itemId=tmdb:movie:7", nil)
	usage, exceeds := tracker.WouldExceedAccountLimit(other, "/debrid/torbox/7/Other.mkv", "acct1", 1)
	if !exceeds || usage.CurrentStreams != 1 {
		t.Fatalf("usage = %+v exceeds = %v, want rejection at 1/1", usage, exceeds)
	}
}
