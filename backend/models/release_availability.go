package models

import (
	"encoding/json"
	"strings"
	"time"
)

// Availability labels returned on titles in search and shelf responses. They
// let clients render "In Theaters" / "Coming Soon" / "Unreleased" badges
// without per-title release or series-details lookups.
const (
	AvailabilityReleased   = "released"    // watchable at home (or a series has aired)
	AvailabilityTheatrical = "theatrical"  // movie currently in theaters only
	AvailabilityComingSoon = "coming_soon" // not out yet; releaseDate within ComingSoonWindow
	AvailabilityUnreleased = "unreleased"  // not out yet; no date or date further out
)

// ComingSoonWindow is how far ahead a not-yet-released title counts as
// "coming soon" rather than "unreleased".
const ComingSoonWindow = 30 * 24 * time.Hour

// MarshalJSON derives Availability (and fills ReleaseDate from release windows
// when known) at encode time so the label stays correct for titles served
// from long-lived caches.
func (t Title) MarshalJSON() ([]byte, error) {
	type titleAlias Title
	alias := titleAlias(t)
	(*Title)(&alias).ApplyReleaseAvailability(time.Now())
	return json.Marshal(alias)
}

// MarshalJSON derives Availability for watchlist items (see Title.MarshalJSON).
func (w WatchlistItem) MarshalJSON() ([]byte, error) {
	type watchlistAlias WatchlistItem
	alias := watchlistAlias(w)
	alias.ReleaseDate = firstNonEmpty(MovieWindowReleaseDate(alias.Theatrical, alias.HomeRelease, nil), alias.ReleaseDate)
	alias.Availability = ReleaseAvailability(alias.MediaType, alias.Status, alias.ReleaseDate, alias.Theatrical, alias.HomeRelease, nil, alias.Year, time.Now())
	return json.Marshal(alias)
}

// ApplyReleaseAvailability fills ReleaseDate from the movie's release windows
// (when present) and recomputes Availability relative to now.
func (t *Title) ApplyReleaseAvailability(now time.Time) {
	if t == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(t.MediaType), "movie") {
		if windowDate := MovieWindowReleaseDate(t.Theatrical, t.HomeRelease, t.Releases); windowDate != "" {
			t.ReleaseDate = windowDate
		}
	}
	t.ReleaseDate = NormalizeReleaseDate(t.ReleaseDate)
	t.Availability = ReleaseAvailability(t.MediaType, t.Status, t.ReleaseDate, t.Theatrical, t.HomeRelease, t.Releases, t.Year, now)
}

// ReleaseAvailability computes the availability label for a title. Release
// windows win over the coarse status; the release date resolves "not out yet"
// into coming_soon vs unreleased and corrects stale cached statuses whose date
// has since passed. Returns "" when nothing is known (or for non-movie/series).
func ReleaseAvailability(mediaType, status, releaseDate string, theatrical, home *Release, releases []Release, year int, now time.Time) string {
	status = strings.ToLower(strings.TrimSpace(status))
	date, hasDate := parseReleaseDate(releaseDate)
	notYet := func() string {
		if hasDate && !date.After(now.Add(ComingSoonWindow)) {
			return AvailabilityComingSoon
		}
		return AvailabilityUnreleased
	}

	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "movie":
		effective := status
		switch {
		case theatrical != nil || home != nil || len(releases) > 0:
			effective = MovieReleaseStatus(Title{MediaType: "movie", Theatrical: theatrical, HomeRelease: home, Releases: releases, ReleaseDate: releaseDate, Year: year})
		case hasDate && status != MovieReleaseStatusReleased:
			// The date is authoritative over a coarse (possibly stale cached) status.
			effective = MovieReleaseStatusFromReleaseDate(releaseDate)
		}
		switch effective {
		case MovieReleaseStatusReleased:
			return AvailabilityReleased
		case MovieReleaseStatusTheatrical:
			return AvailabilityTheatrical
		case MovieReleaseStatusUpcoming:
			return notYet()
		}
		return ""
	case "series", "tv", "show":
		switch {
		case status == SeriesReleaseStatusReleased:
			return AvailabilityReleased
		case hasDate && !date.After(now):
			return AvailabilityReleased
		case status == SeriesReleaseStatusUnreleased || hasDate:
			return notYet()
		}
		return ""
	}
	return ""
}

// MovieWindowReleaseDate returns the earliest date (YYYY-MM-DD) the movie is
// watchable anywhere: theatrical or home windows. Festival premieres are
// ignored. Returns "" when no window has a parseable date.
func MovieWindowReleaseDate(theatrical, home *Release, releases []Release) string {
	var best time.Time
	consider := func(r *Release) {
		if r == nil {
			return
		}
		switch strings.ToLower(strings.TrimSpace(r.Type)) {
		case "premiere":
			return
		}
		if ts, ok := parseReleaseDate(r.Date); ok && (best.IsZero() || ts.Before(best)) {
			best = ts
		}
	}
	consider(theatrical)
	consider(home)
	for i := range releases {
		switch strings.ToLower(strings.TrimSpace(releases[i].Type)) {
		case "theatrical", "theatricallimited", "digital", "physical", "tv":
			consider(&releases[i])
		}
	}
	if best.IsZero() {
		return ""
	}
	return best.Format("2006-01-02")
}

// SeriesPremiereDate returns the earliest regular-season episode air date
// (YYYY-MM-DD), or "" when none is known.
func SeriesPremiereDate(seasons []SeriesSeason) string {
	var best time.Time
	for _, season := range seasons {
		if season.Number <= 0 {
			continue
		}
		for _, episode := range season.Episodes {
			ts, ok := parseReleaseDate(episode.AiredDate)
			if !ok {
				ts, ok = parseReleaseDate(episode.AiredDateTimeUTC)
			}
			if ok && (best.IsZero() || ts.Before(best)) {
				best = ts
			}
		}
	}
	if best.IsZero() {
		return ""
	}
	return best.Format("2006-01-02")
}

// NormalizeReleaseDate trims a provider date/timestamp to YYYY-MM-DD, or ""
// when it cannot be parsed.
func NormalizeReleaseDate(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) >= len("2006-01-02") {
		if _, err := time.Parse("2006-01-02", trimmed[:len("2006-01-02")]); err == nil {
			return trimmed[:len("2006-01-02")]
		}
	}
	if ts, ok := parseReleaseDate(trimmed); ok {
		return ts.UTC().Format("2006-01-02")
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
