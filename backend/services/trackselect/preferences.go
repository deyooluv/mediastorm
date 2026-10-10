package trackselect

import (
	"log"
	"strings"

	"novastream/config"
	"novastream/models"
)

// ConfigLoader loads global settings (the server-wide playback defaults).
type ConfigLoader interface {
	Load() (config.Settings, error)
}

// UserSettingsProvider loads profile settings merged over defaults.
type UserSettingsProvider interface {
	GetWithDefaults(userID string, defaults models.UserSettings) (models.UserSettings, error)
}

// ClientSettingsProvider loads per-device overrides for a profile.
type ClientSettingsProvider interface {
	Get(clientID, userID string) (*models.ClientFilterSettings, error)
}

// ContentPreferenceProvider loads per-title language overrides for a profile.
type ContentPreferenceProvider interface {
	Get(userID, contentID string) (*models.ContentPreference, error)
}

// Resolver computes effective track preferences with the precedence
// global defaults < profile settings < client settings < per-title preference.
// Every dependency is optional.
type Resolver struct {
	Config             ConfigLoader
	UserSettings       UserSettingsProvider
	ClientSettings     ClientSettingsProvider
	ContentPreferences ContentPreferenceProvider
}

// Resolve returns the effective preferences for a profile, client and title.
// Lookup failures are logged and skipped so playback never fails on them.
func (r *Resolver) Resolve(userID, clientID, titleID string) Preferences {
	if r == nil {
		r = &Resolver{}
	}

	// Built-in defaults apply when no global settings are available.
	defaults := models.DefaultUserSettings()
	if r.Config != nil {
		if global, err := r.Config.Load(); err != nil {
			log.Printf("[trackselect] failed to load global settings: %v", err)
		} else {
			defaults.Playback.PreferredAudioLanguage = global.Playback.PreferredAudioLanguage
			defaults.Playback.PreferredSubtitleLanguage = global.Playback.PreferredSubtitleLanguage
			defaults.Playback.AllowedTrackLanguages = models.StringSlicePtr(global.Playback.AllowedTrackLanguages)
			defaults.Playback.PreferredSubtitleMode = global.Playback.PreferredSubtitleMode
		}
	}
	playback := defaults.Playback

	userID = strings.TrimSpace(userID)
	if r.UserSettings != nil && userID != "" {
		if settings, err := r.UserSettings.GetWithDefaults(userID, defaults); err != nil {
			log.Printf("[trackselect] failed to load user settings for %s: %v", userID, err)
		} else {
			playback = settings.Playback
		}
	}

	clientID = strings.TrimSpace(clientID)
	if r.ClientSettings != nil && clientID != "" && userID != "" {
		if client, err := r.ClientSettings.Get(clientID, userID); err != nil {
			log.Printf("[trackselect] failed to load client settings for %s: %v", clientID, err)
		} else if client != nil {
			if client.PreferredAudioLanguage != nil {
				playback.PreferredAudioLanguage = *client.PreferredAudioLanguage
			}
			if client.PreferredSubtitleLanguage != nil {
				playback.PreferredSubtitleLanguage = *client.PreferredSubtitleLanguage
			}
			if client.AllowedTrackLanguages != nil {
				playback.AllowedTrackLanguages = client.AllowedTrackLanguages
			}
			if client.PreferredSubtitleMode != nil {
				playback.PreferredSubtitleMode = *client.PreferredSubtitleMode
			}
		}
	}

	titleID = strings.TrimSpace(titleID)
	if r.ContentPreferences != nil && userID != "" && titleID != "" {
		if pref, err := r.ContentPreferences.Get(userID, titleID); err != nil {
			log.Printf("[trackselect] failed to load content preference for %s/%s: %v", userID, titleID, err)
		} else if pref != nil {
			if audio := SanitizeLanguageCode(pref.AudioLanguage); audio != "" {
				playback.PreferredAudioLanguage = audio
			}
			if subtitle := SanitizeLanguageCode(pref.SubtitleLanguage); subtitle != "" {
				playback.PreferredSubtitleLanguage = subtitle
			}
			if mode := strings.TrimSpace(strings.Trim(pref.SubtitleMode, "'\"")); mode != "" {
				playback.PreferredSubtitleMode = mode
			}
		}
	}

	return PreferencesFromPlayback(playback)
}
