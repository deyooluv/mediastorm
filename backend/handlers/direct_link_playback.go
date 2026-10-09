package handlers

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"novastream/models"
)

// Direct-link playback tracking.
//
// Clients that resolve a provider URL through /api/video/direct-url fetch media
// straight from the debrid CDN, so the backend never sees transport bytes and
// the StreamTracker has nothing to show or count. Those clients still send
// playback progress heartbeats, so a heartbeat whose sourcePath matches a
// direct URL the backend issued is promoted to a direct-link playback.
//
// Requiring an issued direct URL is what keeps this from duplicating proxied
// playback: ordinary app heartbeats name a source the backend is serving itself
// and never match a grant. Overlap with tracked transport streams is still
// resolved in favor of the tracked stream, so a client that falls back from a
// direct link to the proxy is counted once.

// directLinkStreamType is the Active Streams type for direct-link playbacks.
const directLinkStreamType = "direct_link"

const (
	// directLinkHeartbeatTimeout is how long a direct-link playback stays active
	// after its last heartbeat. Clients report every ~10s.
	directLinkHeartbeatTimeout = 30 * time.Second
	// directLinkGrantTTL bounds how long an issued direct URL can be matched by
	// heartbeats. Matching heartbeats refresh it, so long films keep matching.
	directLinkGrantTTL = 12 * time.Hour
)

type directLinkGrant struct {
	accountID string
	expiresAt time.Time
}

// DirectLinkPlayback is a playback the client streams directly from the
// provider, observed only through its progress heartbeats.
type DirectLinkPlayback struct {
	ID            string
	SlotKey       string
	Path          string
	Filename      string
	ProfileID     string
	AccountID     string
	ClientID      string
	ClientIP      string
	MediaMetadata StreamMediaMetadata
	StartTime     time.Time
	LastHeartbeat time.Time
	Position      float64
	Duration      float64
	IsPaused      bool
	IsBuffering   bool
}

// RecordDirectURLIssued remembers that the backend handed out a direct provider
// URL for path, so later heartbeats naming that source can be attributed to a
// direct-link playback.
func (t *StreamTracker) RecordDirectURLIssued(path, accountID string) {
	key := normalizeStreamFailurePath(path)
	if t == nil || key == "" {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneDirectLinksLocked(now)
	if t.directLinkGrants == nil {
		t.directLinkGrants = make(map[string]directLinkGrant)
	}
	t.directLinkGrants[key] = directLinkGrant{
		accountID: strings.TrimSpace(accountID),
		expiresAt: now.Add(directLinkGrantTTL),
	}
}

// ObserveDirectLinkHeartbeat records a progress heartbeat as a direct-link
// playback when its source was issued as a direct URL and no tracked transport
// stream already represents it. It reports whether the heartbeat was
// attributed to a direct-link playback.
func (t *StreamTracker) ObserveDirectLinkHeartbeat(userID, accountID, clientID, clientIP string, update models.PlaybackProgressUpdate) bool {
	if t == nil {
		return false
	}
	userID = strings.TrimSpace(userID)
	pathKey := normalizeStreamFailurePath(update.SourcePath)
	if userID == "" || pathKey == "" || strings.TrimSpace(update.ItemID) == "" || isLiveMediaType(update.MediaType) {
		return false
	}
	slotKey := streamSlotKey(userID, "", clientIP, update.MediaType, update.ItemID, update.SourcePath)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneDirectLinksLocked(now)

	grant, ok := t.directLinkGrants[pathKey]
	if !ok {
		return false
	}
	if grant.accountID != "" && strings.TrimSpace(accountID) != "" && grant.accountID != strings.TrimSpace(accountID) {
		return false
	}
	if update.PlaybackEnded {
		delete(t.directLinkPlaybacks, slotKey)
		return true
	}
	controlKey := playbackControlKey(userID, update.MediaType, update.ItemID)
	if t.trackedStreamCoversLocked(pathKey, controlKey) {
		// The backend is serving this playback itself; the tracked stream is
		// the single source of truth for it.
		delete(t.directLinkPlaybacks, slotKey)
		return false
	}

	grant.expiresAt = now.Add(directLinkGrantTTL)
	t.directLinkGrants[pathKey] = grant

	if t.directLinkPlaybacks == nil {
		t.directLinkPlaybacks = make(map[string]*DirectLinkPlayback)
	}
	playback, exists := t.directLinkPlaybacks[slotKey]
	if !exists {
		playback = &DirectLinkPlayback{
			ID:        fmt.Sprintf("directlink-%d", atomic.AddUint64(&t.counter, 1)),
			SlotKey:   slotKey,
			StartTime: now,
		}
		t.directLinkPlaybacks[slotKey] = playback
	}
	playback.Path = update.SourcePath
	playback.Filename = filepath.Base(pathKey)
	playback.ProfileID = userID
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		playback.AccountID = accountID
	} else {
		playback.AccountID = grant.accountID
	}
	if clientID = strings.TrimSpace(clientID); clientID != "" {
		playback.ClientID = clientID
	}
	playback.ClientIP = clientIP
	playback.MediaMetadata = directLinkMediaMetadata(update)
	playback.LastHeartbeat = now
	playback.Position = update.Position
	playback.Duration = update.Duration
	playback.IsPaused = update.IsPaused
	playback.IsBuffering = update.IsBuffering
	return true
}

// GetActiveDirectLinkPlaybacks returns direct-link playbacks with a fresh
// heartbeat that are not already represented by a tracked transport stream.
func (t *StreamTracker) GetActiveDirectLinkPlaybacks() []DirectLinkPlayback {
	if t == nil {
		return nil
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneDirectLinksLocked(now)

	playbacks := make([]DirectLinkPlayback, 0, len(t.directLinkPlaybacks))
	for _, playback := range t.directLinkPlaybacks {
		if t.directLinkCoveredLocked(playback) {
			continue
		}
		playbacks = append(playbacks, *playback)
	}
	return playbacks
}

// GetDirectLinkPlayback returns the active direct-link playback with id.
func (t *StreamTracker) GetDirectLinkPlayback(id string) (DirectLinkPlayback, bool) {
	for _, playback := range t.GetActiveDirectLinkPlaybacks() {
		if playback.ID == id {
			return playback, true
		}
	}
	return DirectLinkPlayback{}, false
}

func (t *StreamTracker) pruneDirectLinksLocked(now time.Time) {
	for key, grant := range t.directLinkGrants {
		if !grant.expiresAt.After(now) {
			delete(t.directLinkGrants, key)
		}
	}
	for key, playback := range t.directLinkPlaybacks {
		if now.Sub(playback.LastHeartbeat) > directLinkHeartbeatTimeout {
			delete(t.directLinkPlaybacks, key)
		}
	}
}

// activeDirectLinksLocked returns uncovered direct-link playbacks whose
// heartbeat is fresh, without pruning (safe under the read lock).
func (t *StreamTracker) activeDirectLinksLocked(include func(*DirectLinkPlayback) bool) []*DirectLinkPlayback {
	now := time.Now()
	var active []*DirectLinkPlayback
	for _, playback := range t.directLinkPlaybacks {
		if now.Sub(playback.LastHeartbeat) > directLinkHeartbeatTimeout {
			continue
		}
		if include != nil && !include(playback) {
			continue
		}
		if t.directLinkCoveredLocked(playback) {
			continue
		}
		active = append(active, playback)
	}
	return active
}

func (t *StreamTracker) directLinkCoveredLocked(playback *DirectLinkPlayback) bool {
	return t.trackedStreamCoversLocked(
		normalizeStreamFailurePath(playback.Path),
		playbackControlKey(playback.ProfileID, playback.MediaMetadata.MediaType, playback.MediaMetadata.ItemID),
	)
}

// trackedStreamCoversLocked reports whether a backend-served transport stream
// already represents the playback identified by source path or profile+media.
func (t *StreamTracker) trackedStreamCoversLocked(pathKey, controlKey string) bool {
	for _, stream := range t.streams {
		if pathKey != "" && normalizeStreamFailurePath(stream.Path) == pathKey {
			return true
		}
		for _, key := range streamPlaybackControlKeys(stream) {
			if key == controlKey {
				return true
			}
		}
	}
	return false
}

func directLinkMediaMetadata(update models.PlaybackProgressUpdate) StreamMediaMetadata {
	meta := StreamMediaMetadata{
		MediaType:     strings.ToLower(strings.TrimSpace(update.MediaType)),
		ItemID:        strings.ToLower(strings.TrimSpace(update.ItemID)),
		Year:          update.Year,
		SeasonNumber:  update.SeasonNumber,
		EpisodeNumber: update.EpisodeNumber,
		EpisodeName:   update.EpisodeName,
		SeriesID:      update.SeriesID,
		SeriesName:    update.SeriesName,
		MovieName:     update.MovieName,
		PosterURL:     update.PosterURL,
		ExternalIDs:   update.ExternalIDs,
	}
	if meta.MediaType == "episode" {
		meta.Title = update.SeriesName
	} else {
		meta.Title = update.MovieName
	}
	return meta
}

func isLiveMediaType(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "live", "livetv", "live-tv", "channel", "channels":
		return true
	}
	return false
}

// dashboardDirectLinkPlaybacks returns the active direct-link playbacks that
// are not already represented by an HLS session, for the Active Streams views.
func dashboardDirectLinkPlaybacks(hls *HLSManager) []DirectLinkPlayback {
	playbacks := GetStreamTracker().GetActiveDirectLinkPlaybacks()
	if hls == nil || len(playbacks) == 0 {
		return playbacks
	}
	type hlsKey struct{ path, profile, item string }
	var sessions []hlsKey
	hls.mu.RLock()
	for _, session := range hls.sessions {
		session.mu.RLock()
		sessions = append(sessions,
			hlsKey{normalizeStreamFailurePath(session.Path), strings.ToLower(session.ProfileID), strings.ToLower(session.MediaMetadata.ItemID)},
			hlsKey{normalizeStreamFailurePath(session.OriginalPath), strings.ToLower(session.ProfileID), strings.ToLower(session.MediaMetadata.ItemID)},
		)
		session.mu.RUnlock()
	}
	hls.mu.RUnlock()

	filtered := playbacks[:0]
	for _, playback := range playbacks {
		pathKey := normalizeStreamFailurePath(playback.Path)
		profile := strings.ToLower(playback.ProfileID)
		covered := false
		for _, session := range sessions {
			if (session.path != "" && session.path == pathKey) ||
				(session.item != "" && session.item == playback.MediaMetadata.ItemID && session.profile == profile) {
				covered = true
				break
			}
		}
		if !covered {
			filtered = append(filtered, playback)
		}
	}
	return filtered
}
