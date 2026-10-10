package models

import "time"

type ContentServiceType string

const (
	ServiceTypeUnknown ContentServiceType = ""
	ServiceTypeUsenet  ContentServiceType = "usenet"
	ServiceTypeDebrid  ContentServiceType = "debrid"
)

// NZBResult represents a normalized search result from a Torznab/Newznab indexer.
type NZBResult struct {
	Title        string             `json:"title"`
	Indexer      string             `json:"indexer"`
	GUID         string             `json:"guid"`
	Link         string             `json:"link"`
	DownloadURL  string             `json:"downloadUrl"`
	SizeBytes    int64              `json:"sizeBytes"`
	PublishDate  time.Time          `json:"publishDate"`
	Categories   []string           `json:"categories,omitempty"`
	Attributes   map[string]string  `json:"attributes,omitempty"`
	ServiceType  ContentServiceType `json:"serviceType,omitempty"`
	EpisodeCount int                `json:"episodeCount,omitempty"` // Number of episodes in pack (0 if not a pack)
	SizePerFile  bool               `json:"sizePerFile,omitempty"`  // True when sizeBytes is per-file (Stremio scrapers), false when total pack
}

// EffectiveItemSizeBytes returns the size represented by one playable item.
// SizeBytes remains the source-reported value so clients can also show the
// total pack size. Indexer-style sources generally report a pack total, while
// Stremio-style sources with a file index generally report the selected file.
func (r NZBResult) EffectiveItemSizeBytes() int64 {
	if r.SizeBytes <= 0 {
		return r.SizeBytes
	}
	if r.EpisodeCount > 1 && !r.SizePerFile {
		return r.SizeBytes / int64(r.EpisodeCount)
	}
	return r.SizeBytes
}

// ScoreBreakdownItem describes one ranking criterion. Points is the legacy
// human-readable score contribution; RankValue is the exact higher-is-better
// value used when comparing this criterion.
type ScoreBreakdownItem struct {
	Criterion string `json:"criterion"` // Display name of the criterion
	Points    int    `json:"points"`    // Points awarded (positive or negative)
	RankValue int64  `json:"rankValue"` // Exact comparison value; higher ranks first
	Reason    string `json:"reason"`    // Human-readable explanation
}

// ScoredNZBResult extends NZBResult with filter status, scoring breakdown, and rejection reason.
type ScoredNZBResult struct {
	NZBResult
	FilterStatus   string               `json:"filterStatus"`             // "passed" or "filtered"
	FilterReason   string               `json:"filterReason,omitempty"`   // Reason for exclusion (empty if passed)
	TotalScore     int                  `json:"totalScore"`               // Sum of all scoring points
	ScoreBreakdown []ScoreBreakdownItem `json:"scoreBreakdown,omitempty"` // Per-criterion scoring details
	// SourceHealth is the server-side availability annotation (debrid cache
	// state / remembered usenet health). Only populated when the search was
	// requested with includeSourceHealth=true.
	SourceHealth *SourceHealth `json:"sourceHealth,omitempty"`
}

// Source health annotation states.
const (
	SourceHealthHealthy   = "healthy"    // usenet: last health check passed
	SourceHealthUnhealthy = "unhealthy"  // usenet: last health check failed
	SourceHealthCached    = "cached"     // debrid: instantly available
	SourceHealthNotCached = "not_cached" // debrid: not instantly available
	SourceHealthPending   = "pending"    // check still running; poll with Token
	SourceHealthUnknown   = "unknown"    // not checked / no safe quick check
)

// SourceHealth annotates a search result with health or cache state.
type SourceHealth struct {
	State     string     `json:"state"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	// Token identifies the background check for pending annotations; poll
	// GET /api/indexers/source-health?token=... for the final state.
	Token    string `json:"token,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Status is the raw checker status (e.g. debrid "cached"/"not_cached"/"skipped").
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}
