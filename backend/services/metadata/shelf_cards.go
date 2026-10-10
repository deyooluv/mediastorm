package metadata

import (
	"context"
	"fmt"
	"sync"
	"time"

	"novastream/models"
)

const shelfSourceTTL = 10 * time.Minute

func (s *Service) cachedCustomListSource(ctx context.Context, listURL string) ([]mdblistItem, error) {
	var items []mdblistItem
	err := s.fetchShelfCached(ctx, cacheKey("shelf-source", "v1", listURL), shelfSourceTTL, &items, func(ctx context.Context) (any, error) {
		var result []mdblistItem
		err := s.client.fetchMDBListJSONContext(ctx, listURL, &result)
		return result, err
	})
	return items, err
}

func (s *Service) GetCustomListSource(ctx context.Context, listURL string) ([]CuratedItem, error) {
	raw, err := s.cachedCustomListSource(ctx, resolveStreamingListURL(listURL))
	if err != nil {
		return nil, err
	}
	items := make([]CuratedItem, len(raw))
	for i, item := range raw {
		items[i] = CuratedItem{Title: item.Title, Year: item.ReleaseYear, IMDBID: item.IMDBID, MediaType: mdblistItemMediaType(item)}
		if item.TMDBID != nil {
			items[i].TMDBID = *item.TMDBID
		}
		if item.TVDBID != nil {
			items[i].TVDBID = *item.TVDBID
		}
	}
	return items, nil
}

func curatedRaw(item CuratedItem, rank int) mdblistItem {
	raw := mdblistItem{Rank: rank, Title: item.Title, ReleaseYear: item.Year, IMDBID: item.IMDBID, MediaType: curatedItemMediaType(item.MediaType)}
	if item.TMDBID > 0 {
		id := item.TMDBID
		raw.TMDBID = &id
	}
	if item.TVDBID > 0 {
		id := item.TVDBID
		raw.TVDBID = &id
	}
	return raw
}

func (s *Service) shelfCardKey(item CuratedItem) string {
	raw := curatedRaw(item, 0)
	return cacheKey("shelf-card", "v1", s.client.language, raw.MediaType, customListItemTitleID(raw, raw.MediaType))
}

// GetShelfCards loads only the supplied page. Stable provider IDs and source
// artwork survive hydration; optional images, credits and episodes are excluded.
func (s *Service) GetShelfCards(ctx context.Context, items []CuratedItem) ([]models.TrendingItem, error) {
	result := make([]models.TrendingItem, len(items))
	missing := make([]int, 0, len(items))
	for i, item := range items {
		result[i] = buildLiteCustomListItem(curatedRaw(item, i))
		var title models.Title
		if ok, _ := s.cache.get(s.shelfCardKey(item), &title); ok && title.Poster != nil {
			result[i].Title = title
		} else if item.PosterURL != "" {
			result[i].Title.Poster = &models.Image{URL: item.PosterURL, Type: "poster"}
			if item.BackdropURL != "" {
				result[i].Title.Backdrop = &models.Image{URL: item.BackdropURL, Type: "backdrop"}
			}
			result[i].Title.Overview = item.Overview
			result[i].Title.Genres = item.Genres
		} else {
			missing = append(missing, i)
		}
	}
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, idx := range missing {
		if result[idx].Title.Poster != nil {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var title models.Title
			err := s.fetchShelfCached(ctx, s.shelfCardKey(items[i]), 24*time.Hour, &title, func(fetchCtx context.Context) (any, error) {
				raw := curatedRaw(items[i], i)
				base := buildLiteCustomListItem(raw).Title
				id := items[i].TMDBID
				if s.tmdb != nil && s.tmdb.isConfigured() {
					if id <= 0 && base.MediaType == "movie" && base.IMDBID != "" {
						id = s.getTMDBIDForIMDB(fetchCtx, base.IMDBID)
					}
					if id <= 0 && base.MediaType == "series" {
						id = s.resolveTMDBSeriesID(fetchCtx, models.SeriesDetailsQuery{Name: base.Name, Year: base.Year, IMDBID: base.IMDBID, TVDBID: base.TVDBID})
					}
					if id <= 0 && base.MediaType == "movie" && base.IMDBID == "" {
						id = s.resolveTMDBMovieByTitleYear(fetchCtx, base.Name, base.Year)
					}
					if id > 0 {
						var found *models.Title
						var err error
						if base.MediaType == "movie" {
							found, err = s.tmdb.movieDetails(fetchCtx, id)
						} else {
							found, err = s.cachedTMDBShelfSeries(fetchCtx, id)
						}
						if err != nil {
							return nil, err
						}
						if found != nil {
							base = *found
							if base.IMDBID == "" {
								base.IMDBID = items[i].IMDBID
							}
							if base.TVDBID == 0 {
								base.TVDBID = items[i].TVDBID
							}
						}
					}
				}
				if base.Poster == nil && s.client.isConfigured() {
					base = s.enrichLiteCustomListItem(withDeferredShelfArtwork(fetchCtx), raw).Title
				}
				if base.Poster == nil {
					return nil, fmt.Errorf("card metadata unavailable")
				}
				return base, nil
			})
			if err == nil {
				result[i].Title = title
			}
		}(idx)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, nil
}

// Keep the established digital-release and first-air rules. Never infer movie
// availability from a catalog's generic release date or "released" status.
func (s *Service) cachedTMDBShelfSeries(ctx context.Context, id int64) (*models.Title, error) {
	var title models.Title
	err := s.fetchShelfCached(ctx, cacheKey("shelf-tmdb-series", "v1", s.client.language, fmt.Sprint(id)), 24*time.Hour, &title, func(ctx context.Context) (any, error) { return s.tmdb.seriesDetails(ctx, id) })
	return &title, err
}

func (s *Service) FilterShelfVisibility(ctx context.Context, items []models.TrendingItem, movies, shows bool) []models.TrendingItem {
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	keep := make([]bool, len(items))
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			title := &items[i].Title
			if title.MediaType == "movie" && movies {
				id := title.TMDBID
				if id <= 0 && title.IMDBID != "" {
					id = s.getTMDBIDForIMDB(ctx, title.IMDBID)
				}
				s.enrichMovieReleases(ctx, title, id)
				keep[i] = models.MovieReleaseStatus(*title) == models.MovieReleaseStatusReleased
			} else if title.MediaType == "series" && shows {
				id := title.TMDBID
				if id <= 0 {
					id = s.resolveTMDBSeriesID(ctx, models.SeriesDetailsQuery{Name: title.Name, Year: title.Year, IMDBID: title.IMDBID, TVDBID: title.TVDBID})
				}
				if id > 0 && s.tmdb != nil {
					if full, err := s.cachedTMDBShelfSeries(ctx, id); err == nil {
						title.Status = full.Status
						if full.ReleaseDate != "" {
							title.ReleaseDate = full.ReleaseDate
						}
					}
				}
				keep[i] = title.Status == models.SeriesReleaseStatusReleased
			} else {
				keep[i] = true
			}
		}(i)
	}
	wg.Wait()
	result := make([]models.TrendingItem, 0, len(items))
	for i, item := range items {
		if keep[i] {
			result = append(result, item)
		}
	}
	return result
}

// CompleteShelfCards persists rich metadata per title, so returning home does
// not temporarily downgrade a previously hydrated card to its base artwork.
func (s *Service) CompleteShelfCards(ctx context.Context, items []CuratedItem, label string) ([]models.TrendingItem, error) {
	result, err := s.GetCuratedList(ctx, items, label)
	if err != nil {
		return nil, err
	}
	if len(result) == len(items) && ctx.Err() == nil {
		for i, item := range items {
			if result[i].Title.Poster != nil {
				title := result[i].Title
				_ = s.cache.set(s.shelfCardKey(item), title)
				if title.TMDBID > 0 {
					_ = s.cache.set(s.shelfCardKey(CuratedItem{TMDBID: title.TMDBID, MediaType: title.MediaType}), title)
				}
				if title.IMDBID != "" {
					_ = s.cache.set(s.shelfCardKey(CuratedItem{IMDBID: title.IMDBID, MediaType: title.MediaType}), title)
				}
			}
		}
	}
	return result, ctx.Err()
}
