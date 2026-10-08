package handlers

import (
	"net"
	"net/url"
	"strings"

	"novastream/config"
	"novastream/internal/requestsecurity"
)

func configuredProviderHostPolicy(configManager ConfigProvider) requestsecurity.RestrictedHostPolicy {
	allowed := make(map[string]struct{})
	addURLOrigin := func(raw string) {
		if key, ok := providerOriginKey(raw); ok {
			allowed[key] = struct{}{}
		}
	}
	if configManager != nil {
		if settings, err := configManager.Load(); err == nil {
			for _, origin := range settings.Server.AllowedPrivateMediaOrigins {
				addURLOrigin(origin)
			}
			for _, engine := range settings.UsenetEngines {
				if engine.Enabled {
					addURLOrigin(engine.BaseURL)
					addURLOrigin(engine.WebDAVBaseURL)
				}
			}
			for _, indexer := range settings.Indexers {
				if indexer.Enabled {
					addURLOrigin(indexer.URL)
				}
			}
			for _, scraper := range settings.TorrentScrapers {
				if scraper.Enabled {
					addURLOrigin(scraper.URL)
				}
			}
			if strings.EqualFold(settings.Live.Mode, "hdhomerun") {
				lineup, _ := config.HDHomeRunURL(settings.Live.HDHomeRunHost, "/lineup.m3u")
				addURLOrigin(lineup)
			}
			addURLOrigin(settings.Live.PlaylistURL)
			addURLOrigin(settings.Live.ManifestURL)
			addURLOrigin(settings.Live.XtreamHost)
			addURLOrigin(settings.Live.StalkerPortalURL)
			for _, source := range append(settings.Live.Sources, settings.Live.PlaylistSources...) {
				if source.Enabled == nil || *source.Enabled {
					if strings.EqualFold(source.Mode, "hdhomerun") {
						lineup, _ := config.HDHomeRunURL(source.HDHomeRunHost, "/lineup.m3u")
						addURLOrigin(lineup)
					}
					addURLOrigin(source.PlaylistURL)
					addURLOrigin(source.ManifestURL)
					addURLOrigin(source.XtreamHost)
					addURLOrigin(source.StalkerPortalURL)
				}
			}
		}
	}
	return func(hostname, port string) bool {
		_, ok := allowed[privateMediaEndpointKey(hostname, port)]
		return ok
	}
}

// providerOriginKey returns the host:port endpoint key for an HTTP(S) URL,
// applying the scheme's default port when none is given.
func providerOriginKey(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Hostname() == "" {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", false
		}
	} else if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return "", false
	}
	return privateMediaEndpointKey(parsed.Hostname(), port), true
}

func privateMediaEndpointKey(hostname, port string) string {
	hostname = strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(hostname), "[]"), "."))
	return net.JoinHostPort(hostname, strings.TrimSpace(port))
}
