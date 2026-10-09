package mediaidentity

import (
	"context"
	"fmt"
	"net/http"
	"novastream/models"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MappingSnapshot retains upstream data and validators across server restarts.
type MappingSnapshot struct {
	Key       string
	Body      []byte
	ETag      string
	Modified  string
	CheckedAt time.Time
}
type MappingRepository interface {
	Get(context.Context, string) (*MappingSnapshot, error)
	Put(context.Context, MappingSnapshot) error
}
type mappingEntry struct {
	snapshot MappingSnapshot
	data     any
	retry    time.Time
}
type EpisodeMappingService struct {
	mu               sync.Mutex
	entries          map[string]mappingEntry
	pending          map[string]chan struct{}
	repo             MappingRepository
	client           *http.Client
	animeURL, xemURL string
}

var episodeMappings atomic.Pointer[EpisodeMappingService]

func NewEpisodeMappingService(repo MappingRepository, client *http.Client) *EpisodeMappingService {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &EpisodeMappingService{repo: repo, client: client, entries: map[string]mappingEntry{}, pending: map[string]chan struct{}{}, animeURL: "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-master.xml", xemURL: "https://thexem.info/map/all"}
}

// SetEpisodeMappingService installs the shared mapping cache during startup.
func SetEpisodeMappingService(s *EpisodeMappingService) *EpisodeMappingService {
	return episodeMappings.Swap(s)
}

// Run refreshes the small set of sources actually used by this server. Cached
// snapshots remain usable during outages; interactive lookups never await a
// refresh if any validated snapshot is already available.
func (s *EpisodeMappingService) Run(ctx context.Context) {
	s.load(ctx, "anime-lists", s.animeURL, parseAnimeMappings)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.load(ctx, "anime-lists", s.animeURL, parseAnimeMappings)
			s.mu.Lock()
			var keys []string
			for k := range s.entries {
				if strings.HasPrefix(k, "xem:") {
					keys = append(keys, k)
				}
			}
			s.mu.Unlock()
			for _, k := range keys {
				if ctx.Err() != nil {
					return
				}
				s.load(ctx, k, s.xemURL+"?origin=tvdb&id="+strings.TrimPrefix(k, "xem:"), parseXEMMappings)
			}
		}
	}
}
func EnsureEpisodeMappings(ctx context.Context, titleID string, season, episode int, isAnime bool, numbering ...*models.EpisodeNumbering) {
	titleID = mappingNumberingID(titleID, numbering)
	if titleID == "" {
		return
	}
	s := episodeMappings.Load()
	if s == nil || season < 0 || episode <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if isAnime {
		s.load(ctx, "anime-lists", s.animeURL, parseAnimeMappings)
	}
	tvdb, _, ok := s.tvdbEpisode(titleID, EpisodeCoordinate{season, episode})
	if ok {
		xemCtx, cancelXEM := context.WithTimeout(ctx, 4*time.Second)
		defer cancelXEM()
		key := "xem:" + strconv.FormatInt(tvdb, 10)
		s.load(xemCtx, key, s.xemURL+"?origin=tvdb&id="+strconv.FormatInt(tvdb, 10), parseXEMMappings)
	}
}
func (s *EpisodeMappingService) tvdbEpisode(titleID string, c EpisodeCoordinate) (int64, EpisodeCoordinate, bool) {
	provider, id := SeriesProviderAndID(titleID)
	if provider == "tvdb" {
		n, err := strconv.ParseInt(id, 10, 64)
		return n, c, err == nil && n > 0
	}
	s.mu.Lock()
	entry := s.entries["anime-lists"]
	s.mu.Unlock()
	if idx, ok := entry.data.(animeIndex); ok {
		return idx.resolve(titleID, c)
	}
	return 0, EpisodeCoordinate{}, false
}

// ReleaseEpisodeAliases is read-only and preserves catalog identity. Anime and
// XEM aliases intentionally do not enter KnownAnthologyEpisode/scrobble paths.
func ReleaseEpisodeAliases(titleID string, season, episode int, numbering ...*models.EpisodeNumbering) []AnthologyEpisode {
	titleID = mappingNumberingID(titleID, numbering)
	if titleID == "" {
		return nil
	}
	var out []AnthologyEpisode
	if m, ok := KnownAnthologyEpisode(titleID, season, episode); ok {
		out = append(out, m)
	}
	if m, ok := seasonOffsetAlias(seriesSeasonOffsets, titleID, season, episode); ok {
		out = append(out, m)
	}
	s := episodeMappings.Load()
	if s == nil || season < 1 || episode < 1 {
		return out
	}
	c := EpisodeCoordinate{season, episode}
	tvdb, target, ok := s.tvdbEpisode(titleID, c)
	if !ok || target.Season < 1 || target.Episode < 1 {
		return out
	}
	add := func(coord EpisodeCoordinate, source string, absolute int) {
		if coord.Season < 1 || coord.Episode < 1 {
			return
		} // special-to-regular identities are not interchangeable
		if coord == c {
			return
		}
		for i, v := range out {
			if v.Season == coord.Season && v.Episode == coord.Episode {
				if absolute > 0 && v.AbsoluteEpisode == 0 && v.Source != "" {
					out[i].AbsoluteEpisode = absolute
					out[i].Source += "+" + source
				}
				return
			}
		}
		providerID := tvdb
		if source == "thexem" {
			providerID = 0
		} // scene coordinates cannot be queried as TVDB coordinates
		out = append(out, AnthologyEpisode{TVDBID: providerID, Season: coord.Season, Episode: coord.Episode, Source: source, AbsoluteEpisode: absolute})
	}
	add(target, "anime-lists", 0)
	// TVDB catalog episodes also need releases using TMDB's cour numbering.
	// Check the global round trip to exclude unbounded earlier-cour defaults.
	s.mu.Lock()
	anime := s.entries["anime-lists"]
	s.mu.Unlock()
	if idx, ok := anime.data.(animeIndex); ok && strings.HasPrefix(titleID, "tvdb:series:") {
		if tmdb, reverse, valid := idx.resolveTo(titleID, c, true); valid {
			reverseID := fmt.Sprintf("tmdb:tv:%d", tmdb)
			if backID, back, valid := idx.resolve(reverseID, reverse); valid && backID == tvdb && back == c && reverse != c {
				out = append(out, AnthologyEpisode{Season: reverse.Season, Episode: reverse.Episode, Source: "anime-lists", Numbering: &models.EpisodeNumbering{SeriesID: reverseID, Ordering: "official"}})
			}
		}
	}
	s.mu.Lock()
	entry := s.entries[fmt.Sprintf("xem:%d", tvdb)]
	s.mu.Unlock()
	if x, ok := entry.data.(xemIndex); ok {
		if mapped, ok := x[target]; ok {
			add(mapped.Coordinate, "thexem", mapped.Absolute)
		}
	}
	return out
}

// Community mappings describe aired/official order, never DVD or custom orders.
func mappingNumberingID(titleID string, numbering []*models.EpisodeNumbering) string {
	if len(numbering) == 0 || numbering[0] == nil {
		return titleID
	}
	n := numbering[0]
	if n.Ordering != "" && n.Ordering != "official" {
		return ""
	}
	provider, id := SeriesProviderAndID(n.SeriesID)
	numericID, err := strconv.ParseInt(id, 10, 64)
	if (provider != "tvdb" && provider != "tmdb") || err != nil || numericID <= 0 {
		return ""
	}
	return n.SeriesID
}
