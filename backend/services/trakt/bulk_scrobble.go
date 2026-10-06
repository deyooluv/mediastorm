package trakt

import (
	"time"

	"novastream/internal/watchsync"
	"novastream/models"
)

// SyncWatchHistory uses the history endpoints with explicit episode lists;
// never a show-only payload, which could mark episodes outside the selection.
func (s *Scrobbler) SyncWatchHistory(userID string, items []models.WatchHistoryItem) error {
	if len(items) == 0 || !s.IsEnabledForUser(userID) {
		return nil
	}
	token, err := s.getAccessTokenForUser(userID)
	if err != nil || token == "" {
		return err
	}
	account := s.getAccountForUser(userID)
	if account == nil {
		return nil
	}
	// Keep account credentials local to this operation.
	client := s.client.ForAccount(account)
	for _, group := range watchsync.Groups(items) {
		for start := 0; start < len(group); start += watchsync.BatchSize {
			chunk := group[start:min(start+watchsync.BatchSize, len(group))]
			req := bulkHistoryRequest(chunk)
			if len(req.Movies) == 0 && len(req.Shows) == 0 {
				continue
			}
			var resp *SyncHistoryResponse
			if group[0].Watched {
				resp, err = client.AddToHistory(token, req)
			} else {
				resp, err = client.RemoveFromHistory(token, req)
			}
			if err != nil {
				return err
			}
			// Only retry explicitly rejected episodes. Replaying the whole batch
			// would create additional Trakt plays for accepted episodes.
			var retry []models.WatchHistoryItem
			if resp != nil {
				for _, show := range resp.NotFound.Shows {
					for _, season := range show.Seasons {
						for _, ep := range season.Episodes {
							for _, item := range chunk {
								if item.MediaType != "episode" || item.SeasonNumber != season.Number || item.EpisodeNumber != ep.Number {
									continue
								}
								absolute := traktAbsoluteEpisodeNumber(item.EpisodeNumber, item.ExternalIDs)
								if absolute == item.EpisodeNumber {
									continue
								}
								item.EpisodeNumber = absolute
								retry = append(retry, item)
							}
						}
					}
				}
			}
			if len(retry) > 0 {
				if group[0].Watched {
					_, err = client.AddToHistory(token, bulkHistoryRequest(retry))
				} else {
					_, err = client.RemoveFromHistory(token, bulkHistoryRequest(retry))
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func bulkHistoryRequest(items []models.WatchHistoryItem) SyncHistoryRequest {
	var req SyncHistoryRequest
	seasons := make(map[int]int)
	for _, item := range items {
		stamp := ""
		if item.Watched {
			stamp = item.WatchedAt.UTC().Format(time.RFC3339)
		}
		ids := ShowSyncIDs(watchsync.ID(item.ExternalIDs, "tvdb"), item.ExternalIDs)
		if ids == (SyncIDs{}) {
			continue
		}
		if item.MediaType == "movie" {
			req.Movies = append(req.Movies, SyncMovie{IDs: ids, WatchedAt: stamp})
			continue
		}
		if item.MediaType != "episode" || item.SeasonNumber < 0 || item.EpisodeNumber <= 0 {
			continue
		}
		if len(req.Shows) == 0 {
			req.Shows = append(req.Shows, SyncShow{IDs: ids})
		}
		index, exists := seasons[item.SeasonNumber]
		if !exists {
			index = len(req.Shows[0].Seasons)
			seasons[item.SeasonNumber] = index
			req.Shows[0].Seasons = append(req.Shows[0].Seasons, SyncSeason{Number: item.SeasonNumber})
		}
		season := &req.Shows[0].Seasons[index]
		season.Episodes = append(season.Episodes, SyncEpisode{Number: item.EpisodeNumber, WatchedAt: stamp, IDs: episodeSyncIDs(item.ExternalIDs)})
	}
	return req
}
