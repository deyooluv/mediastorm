package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"novastream/config"
	"novastream/internal/auth"
	"novastream/internal/requestsecurity"
)

func newStremioPolicyTestHandler(t *testing.T, shelves ...config.ShelfConfig) *MetadataHandler {
	t.Helper()
	manager := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	settings := config.DefaultSettings()
	settings.HomeShelves.Shelves = append(settings.HomeShelves.Shelves, shelves...)
	if err := manager.Save(settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	return NewMetadataHandler(&fakeMetadataService{}, manager)
}

func stremioManifestRequest(serverURL string, master bool) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/manifest?url="+url.QueryEscape(serverURL+"/configured/manifest.json"), nil)
	return request.WithContext(context.WithValue(request.Context(), auth.ContextKeyIsMaster, master))
}

func TestStremioManifestRejectsPrivateAddonForNonMaster(t *testing.T) {
	server := newStremioShelfTestServer(t)
	defer server.Close()
	handler := newStremioPolicyTestHandler(t)

	response := httptest.NewRecorder()
	handler.StremioManifest(response, stremioManifestRequest(server.URL, false))

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Allowed Private Media Origins") {
		t.Fatalf("expected private-origin guidance, got %s", response.Body.String())
	}
}

func TestStremioManifestAllowsMasterTypedPrivateAddon(t *testing.T) {
	server := newStremioShelfTestServer(t)
	defer server.Close()
	handler := newStremioPolicyTestHandler(t)

	response := httptest.NewRecorder()
	handler.StremioManifest(response, stremioManifestRequest(server.URL, true))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestStremioShelfCatalogAllowsGlobalShelfPrivateOrigin(t *testing.T) {
	server := newStremioShelfTestServer(t)
	defer server.Close()
	manifestURL := server.URL + "/configured/manifest.json"
	handler := newStremioPolicyTestHandler(t, config.ShelfConfig{
		ID: "stremio-ranked", Name: "Ranked", Type: "stremio",
		AddonManifestURL: manifestURL, AddonCatalogType: "movie", AddonCatalogID: "ranked",
	})

	metas, err := handler.loadStremioShelfCatalog(context.Background(), manifestURL, "movie", "ranked")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("meta count=%d, want 3", len(metas))
	}
}

func TestStremioShelfCatalogRejectsUnconfiguredPrivateOrigin(t *testing.T) {
	server := newStremioShelfTestServer(t)
	defer server.Close()
	handler := newStremioPolicyTestHandler(t, config.ShelfConfig{
		ID: "stremio-other", Name: "Other", Type: "stremio",
		AddonManifestURL: "http://10.0.0.4:3002/manifest.json", AddonCatalogType: "movie", AddonCatalogID: "ranked",
	})

	_, err := handler.loadStremioShelfCatalog(context.Background(), server.URL+"/configured/manifest.json", "movie", "ranked")
	if !errors.Is(err, requestsecurity.ErrRestrictedOutboundAddress) {
		t.Fatalf("err=%v, want restricted outbound address", err)
	}
}

func TestStremioShelfHostPolicyHonorsAllowedPrivateMediaOrigins(t *testing.T) {
	manager := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	settings := config.DefaultSettings()
	settings.Server.AllowedPrivateMediaOrigins = []string{"http://aiometadata:3002"}
	if err := manager.Save(settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	policy := NewMetadataHandler(&fakeMetadataService{}, manager).stremioShelfHostPolicy()

	if !policy("aiometadata", "3002") {
		t.Fatal("allowlisted private origin was rejected")
	}
	if policy("aiometadata", "3003") {
		t.Fatal("non-allowlisted port was permitted")
	}
}
