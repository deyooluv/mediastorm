package remotemedia

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"novastream/models"
	"novastream/services/jellyfin"
)

const remotePlaybackReportInterval = 10 * time.Second

type PlaybackUserService interface {
	Get(id string) (models.User, bool)
}

type remotePlaybackSession struct {
	lastReported time.Time
	state        string
	started      bool
}

// PlaybackReporter mirrors strmr's existing player heartbeat to the source
// server when the active stream belongs to a Plex or Jellyfin library.
type PlaybackReporter struct {
	service  *Service
	users    PlaybackUserService
	mu       sync.Mutex
	sessions map[string]remotePlaybackSession
	now      func() time.Time
}

func NewPlaybackReporter(service *Service) *PlaybackReporter {
	return &PlaybackReporter{service: service, sessions: make(map[string]remotePlaybackSession)}
}

// SetUserService supplies profile-to-Plex account associations. Profiles without
// a connected Plex account must not write playback into the library owner's history.
func (r *PlaybackReporter) SetUserService(users PlaybackUserService) {
	r.users = users
}

func (r *PlaybackReporter) HandleProgressUpdate(userID string, update models.PlaybackProgressUpdate, _ float64) {
	r.report(userID, update, false)
}

func (r *PlaybackReporter) StopSession(userID string, update models.PlaybackProgressUpdate, _ float64) {
	r.report(userID, update, true)
}

// ClearSession is used when local watched-state handling takes over at 90%.
// The remote server still needs a live timeline until playback actually ends.
func (r *PlaybackReporter) ClearSession(userID string, update models.PlaybackProgressUpdate) {
	r.report(userID, update, false)
}

func (r *PlaybackReporter) report(userID string, update models.PlaybackProgressUpdate, stopped bool) {
	provider, itemID := remotePlaybackSource(update.SourcePath)
	if r == nil || r.service == nil || provider == "" || itemID == "" {
		return
	}

	state := remotePlaybackState(update, stopped)
	sessionKey := strings.TrimSpace(userID) + "\x00" + provider + "\x00" + itemID
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}

	r.mu.Lock()
	previous := r.sessions[sessionKey]
	if !stopped && previous.started && previous.state == state && now.Sub(previous.lastReported) < remotePlaybackReportInterval {
		r.mu.Unlock()
		return
	}
	if stopped {
		delete(r.sessions, sessionKey)
	} else {
		r.sessions[sessionKey] = remotePlaybackSession{lastReported: now, state: state, started: true}
	}
	r.mu.Unlock()

	item, err := r.service.repo.GetItem(context.Background(), itemID)
	if err != nil || item == nil {
		return
	}
	library, err := r.service.repo.GetLibrary(context.Background(), item.LibraryID)
	if err != nil || library == nil || library.Provider != provider {
		return
	}
	if err := r.reportToProvider(context.Background(), userID, library, item, sessionKey, state, previous.state, !previous.started, update); err != nil {
		log.Printf("[remote-media] %s playback report failed for %s: %v", provider, item.Title, err)
	}
}

func (r *PlaybackReporter) reportToProvider(
	ctx context.Context,
	userID string,
	library *models.RemoteMediaLibrary,
	item *models.RemoteMediaItem,
	sessionKey, state, previousState string,
	first bool,
	update models.PlaybackProgressUpdate,
) error {
	settings, err := r.service.cfg.Load()
	if err != nil {
		return err
	}
	sessionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("strmr-playback\x00"+sessionKey)).String()
	position := time.Duration(max(0, update.Position) * float64(time.Second))
	duration := time.Duration(max(0, update.Duration) * float64(time.Second))

	if library.Provider == models.MediaSourcePlex {
		if r.users == nil {
			return nil
		}
		user, ok := r.users.Get(userID)
		if !ok || strings.TrimSpace(user.PlexAccountID) == "" {
			return nil
		}
		account := settings.Plex.GetAccountByID(user.PlexAccountID)
		if account == nil || strings.TrimSpace(account.AuthToken) == "" {
			return nil
		}
		// Keep the source server/item and verified address, but resolve its access
		// token under the viewing profile's account, including the cache key.
		playbackLibrary := *library
		playbackLibrary.AccountID = account.ID
		server, err := r.service.plexServerForLibrary(&playbackLibrary, account.AuthToken)
		if err != nil {
			return err
		}
		return r.service.plex.ReportTimeline(ctx, server, item.ExternalItemID, sessionID, state, position, duration)
	}

	account := settings.Jellyfin.GetAccountByID(library.AccountID)
	if account == nil {
		return ErrNotFound
	}
	event := "progress"
	if first && state != "stopped" {
		event = "start"
	} else if state == "stopped" {
		event = "stop"
	}
	eventName := "TimeUpdate"
	if state == "paused" {
		eventName = "Pause"
	} else if state == "playing" && previousState == "paused" {
		eventName = "Unpause"
	}
	return r.service.jellyfin.ReportPlayback(ctx, account.ServerURL, account.Token, event, jellyfin.PlaybackReport{
		ItemID:        item.ExternalItemID,
		MediaSourceID: item.ExternalMediaID,
		PlaySessionID: sessionID,
		PositionTicks: int64(max(0, update.Position) * 10_000_000),
		IsPaused:      state == "paused",
		IsMuted:       false,
		CanSeek:       true,
		PlayMethod:    "DirectPlay",
		RepeatMode:    "RepeatNone",
		PlaybackOrder: "Default",
		EventName:     eventName,
	})
}

func remotePlaybackSource(sourcePath string) (provider, itemID string) {
	path := strings.TrimSpace(sourcePath)
	for _, candidate := range []struct {
		prefix   string
		provider string
	}{{"plexmedia:", models.MediaSourcePlex}, {"jellyfinmedia:", models.MediaSourceJellyfin}} {
		if !strings.HasPrefix(path, candidate.prefix) {
			continue
		}
		itemID = strings.TrimSpace(strings.TrimPrefix(path, candidate.prefix))
		if slash := strings.IndexByte(itemID, '/'); slash >= 0 {
			itemID = itemID[:slash]
		}
		return candidate.provider, itemID
	}
	return "", ""
}

func remotePlaybackState(update models.PlaybackProgressUpdate, stopped bool) string {
	if stopped || update.PlaybackEnded {
		return "stopped"
	}
	if update.IsBuffering {
		return "buffering"
	}
	if update.IsPaused {
		return "paused"
	}
	return "playing"
}
