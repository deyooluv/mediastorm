package handlers

import (
	"strings"

	"novastream/models"
	"novastream/services/trackselect"
)

// AudioStreamInfo contains audio stream metadata for track selection
type AudioStreamInfo = models.AudioStreamInfo

// SubtitleStreamInfo contains subtitle stream metadata for track selection
type SubtitleStreamInfo = models.SubtitleStreamInfo

// CompatibleAudioCodecs lists codecs that can be played without transcoding
var CompatibleAudioCodecs = trackselect.CompatibleAudioCodecs

// IsIncompatibleAudioCodec returns true for codecs that need transcoding.
// This includes TrueHD, DTS, FLAC, PCM, and any other codec not in CompatibleAudioCodecs.
// HLS/fMP4 only supports AAC, AC-3, and E-AC-3 for Apple devices.
func IsIncompatibleAudioCodec(codec string) bool {
	c := strings.ToLower(strings.TrimSpace(codec))
	if c == "" {
		return false // Unknown codec, let FFmpeg handle it
	}
	// If not in the compatible list, it needs transcoding
	return !CompatibleAudioCodecs[c]
}

// IsTrueHDCodec returns true specifically for TrueHD/MLP codecs.
func IsTrueHDCodec(codec string) bool {
	return trackselect.IsTrueHDCodec(codec)
}

func IsUnsupportedNativeSubtitleCodec(codec string) bool {
	return trackselect.IsUnsupportedNativeSubtitleCodec(codec)
}

// IsIncompatibleVideoCodec returns true for video codecs that iOS/tvOS cannot play natively.
// iOS only supports H.264 (AVC) and HEVC (H.265). Legacy codecs like MPEG-4 Part 2 (XviD/DivX),
// MPEG-2, VC-1, VP8/VP9, etc. require transcoding to H.264.
func IsIncompatibleVideoCodec(codec string) bool {
	c := strings.ToLower(strings.TrimSpace(codec))
	// Compatible codecs (iOS native support)
	compatibleVideoCodecs := map[string]bool{
		"h264": true, "avc": true, "avc1": true,
		"hevc": true, "h265": true, "hvc1": true, "hev1": true,
	}
	// If empty or compatible, no transcoding needed
	if c == "" || compatibleVideoCodecs[c] {
		return false
	}
	// Any other codec is incompatible and needs transcoding
	return true
}

// IsCommentaryTrack checks if an audio track is a commentary track based on its title
func IsCommentaryTrack(title string) bool {
	return trackselect.IsCommentaryTrack(title)
}

// matchesLanguage checks if a stream matches the preferred language.
func matchesLanguage(language, title, normalizedPref string) bool {
	return trackselect.MatchesLanguage(language, title, normalizedPref)
}

// FindAudioTrackByLanguage finds an audio track matching the preferred language.
// See trackselect.FindAudioTrack. Returns -1 if no matching track is found.
func FindAudioTrackByLanguage(streams []AudioStreamInfo, preferredLanguage string) int {
	return trackselect.FindAudioTrack(streams, preferredLanguage)
}

// FindSubtitleTrackByPreference finds a subtitle track matching the preferences.
// See trackselect.FindSubtitleTrack. Returns -1 if no matching track is found.
func FindSubtitleTrackByPreference(streams []SubtitleStreamInfo, preferredLanguage, mode, audioLanguage string) int {
	return trackselect.FindSubtitleTrack(streams, preferredLanguage, mode, audioLanguage)
}
