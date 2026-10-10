package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"novastream/handlers"
	"novastream/internal/auth"
	"novastream/models"
	"novastream/services/clients"

	"github.com/gorilla/mux"
)

type fakeClientOwnership struct {
	// accountID -> set of profile IDs
	membership map[string]map[string]bool
}

func (f *fakeClientOwnership) BelongsToAccount(profileID, accountID string) bool {
	if f.membership == nil {
		return false
	}
	profiles, ok := f.membership[accountID]
	if !ok {
		return false
	}
	return profiles[profileID]
}

type recordingClientSettings struct{}

func (recordingClientSettings) Get(clientID, userID string) (*models.ClientFilterSettings, error) {
	return nil, nil
}
func (recordingClientSettings) Update(clientID, userID string, settings models.ClientFilterSettings) error {
	return nil
}
func (recordingClientSettings) Delete(clientID, userID string) error { return nil }
func (recordingClientSettings) DeleteByClient(clientID string) error { return nil }
func (recordingClientSettings) Move(clientID, fromUserID, toUserID string) error {
	return nil
}

type capturingClientSettings struct {
	stored *models.ClientFilterSettings
}

func (c *capturingClientSettings) Get(clientID, userID string) (*models.ClientFilterSettings, error) {
	return c.stored, nil
}
func (c *capturingClientSettings) Update(clientID, userID string, settings models.ClientFilterSettings) error {
	c.stored = &settings
	return nil
}
func (c *capturingClientSettings) Delete(clientID, userID string) error { return nil }
func (c *capturingClientSettings) DeleteByClient(clientID string) error { return nil }
func (c *capturingClientSettings) Move(clientID, fromUserID, toUserID string) error {
	return nil
}

func clientsAuthRequest(method, path string, body any, vars map[string]string, accountID string, isMaster bool) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	if len(vars) > 0 {
		r = mux.SetURLVars(r, vars)
	}
	ctx := context.WithValue(r.Context(), auth.ContextKeyAccountID, accountID)
	ctx = context.WithValue(ctx, auth.ContextKeyIsMaster, isMaster)
	return r.WithContext(ctx)
}

func TestClientsHandler_Register_LinksDeviceToAdditionalProfile(t *testing.T) {
	svc, err := clients.NewService(t.TempDir())
	if err != nil {
		t.Fatalf("new clients service: %v", err)
	}

	// Device previously registered under account A's profile.
	if _, err := svc.Register("device-1", "profile-a", "Android TV", "Android", "1.0", "Fire TV", ""); err != nil {
		t.Fatalf("seed register: %v", err)
	}

	ownership := &fakeClientOwnership{
		membership: map[string]map[string]bool{
			"account-a": {"profile-a": true},
			"account-b": {"profile-b": true},
		},
	}
	h := handlers.NewClientsHandler(svc, recordingClientSettings{}, ownership)

	body := map[string]string{
		"id":         "device-1",
		"userId":     "profile-b",
		"deviceType": "Android TV",
		"os":         "Android",
		"appVersion": "1.1",
		"deviceName": "Fire TV",
		"nickname":   "Living Room",
	}
	r := clientsAuthRequest(http.MethodPost, "/api/clients/register", body, nil, "account-b", false)
	w := httptest.NewRecorder()
	h.Register(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}

	var resp struct {
		Client models.Client `json:"client"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Client.UserID != "profile-b" {
		t.Fatalf("userId = %q, want profile-b (last active)", resp.Client.UserID)
	}
	if resp.Client.Nickname != "Living Room" {
		t.Fatalf("nickname = %q, want Living Room", resp.Client.Nickname)
	}
	// Device remains listed under both people.
	if got := svc.ListByUser("profile-a"); len(got) != 1 || got[0].ID != "device-1" {
		t.Fatalf("profile-a devices = %+v, want device-1", got)
	}
	if got := svc.ListByUser("profile-b"); len(got) != 1 || got[0].ID != "device-1" {
		t.Fatalf("profile-b devices = %+v, want device-1", got)
	}
}

func TestClientsHandler_UpdateSettingsStripsVolatileThroughput(t *testing.T) {
	svc, err := clients.NewService(t.TempDir())
	if err != nil {
		t.Fatalf("new clients service: %v", err)
	}
	if _, err := svc.Register("device-1", "profile-a", "Apple TV", "tvOS", "1.0", "TV", ""); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	settings := &capturingClientSettings{}
	ownership := &fakeClientOwnership{membership: map[string]map[string]bool{"account-a": {"profile-a": true}}}
	h := handlers.NewClientsHandler(svc, settings, ownership)
	body := models.ClientFilterSettings{AdaptivePlayback: &models.AdaptivePlaybackSettings{
		MeasuredMbps: models.FloatPtr(29.1),
		MeasuredAt:   models.Int64Ptr(1_700_000_000),
		DisplayHDR:   models.BoolPtr(true),
	}}
	r := clientsAuthRequest(http.MethodPut, "/api/clients/device-1/settings?userId=profile-a", body, map[string]string{"clientID": "device-1"}, "account-a", false)
	w := httptest.NewRecorder()
	h.UpdateSettings(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if settings.stored == nil || settings.stored.AdaptivePlayback == nil {
		t.Fatal("display capability settings were not stored")
	}
	adaptive := settings.stored.AdaptivePlayback
	if adaptive.MeasuredMbps != nil || adaptive.MeasuredAt != nil {
		t.Fatalf("volatile throughput was persisted: %+v", adaptive)
	}
	if adaptive.DisplayHDR == nil || !*adaptive.DisplayHDR {
		t.Fatal("display capability was not preserved")
	}
}

func TestClientsHandler_Register_ReclaimsOrphanedClient(t *testing.T) {
	svc, err := clients.NewService(t.TempDir())
	if err != nil {
		t.Fatalf("new clients service: %v", err)
	}

	// Simulate orphan: client still in memory for a deleted profile.
	if _, err := svc.Register("device-orphan", "deleted-profile", "iPhone", "iOS", "1.0", "iPhone", ""); err != nil {
		t.Fatalf("seed register: %v", err)
	}

	ownership := &fakeClientOwnership{
		membership: map[string]map[string]bool{
			"account-b": {"profile-b": true},
			// deleted-profile is intentionally absent
		},
	}
	h := handlers.NewClientsHandler(svc, recordingClientSettings{}, ownership)

	body := map[string]string{
		"id":         "device-orphan",
		"userId":     "profile-b",
		"deviceType": "iPhone",
		"os":         "iOS",
		"appVersion": "1.0",
		"nickname":   "My Phone",
	}
	r := clientsAuthRequest(http.MethodPost, "/api/clients/register", body, nil, "account-b", false)
	w := httptest.NewRecorder()
	h.Register(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}

	var resp struct {
		Client models.Client `json:"client"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Client.UserID != "profile-b" {
		t.Fatalf("userId = %q, want profile-b", resp.Client.UserID)
	}
	if resp.Client.Nickname != "My Phone" {
		t.Fatalf("nickname = %q, want My Phone", resp.Client.Nickname)
	}
}

func TestClientsHandler_Register_RejectsUnauthorizedTargetProfile(t *testing.T) {
	svc, err := clients.NewService(t.TempDir())
	if err != nil {
		t.Fatalf("new clients service: %v", err)
	}

	ownership := &fakeClientOwnership{
		membership: map[string]map[string]bool{
			"account-b": {"profile-b": true},
		},
	}
	h := handlers.NewClientsHandler(svc, recordingClientSettings{}, ownership)

	body := map[string]string{
		"id":         "device-new",
		"userId":     "profile-a", // not owned by account-b
		"deviceType": "Android TV",
		"os":         "Android",
		"appVersion": "1.0",
		"nickname":   "Nope",
	}
	r := clientsAuthRequest(http.MethodPost, "/api/clients/register", body, nil, "account-b", false)
	w := httptest.NewRecorder()
	h.Register(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "user not found" {
		t.Fatalf("error = %q, want user not found", resp["error"])
	}
}

func TestClientsHandler_Update_NicknameRequiresExistingClient(t *testing.T) {
	svc, err := clients.NewService(t.TempDir())
	if err != nil {
		t.Fatalf("new clients service: %v", err)
	}

	ownership := &fakeClientOwnership{
		membership: map[string]map[string]bool{
			"account-b": {"profile-b": true},
		},
	}
	h := handlers.NewClientsHandler(svc, recordingClientSettings{}, ownership)

	nickname := "Bedroom"
	body := map[string]any{"nickname": nickname}
	r := clientsAuthRequest(http.MethodPut, "/api/clients/missing", body, map[string]string{"clientID": "missing"}, "account-b", false)
	w := httptest.NewRecorder()
	h.Update(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", w.Code, w.Body.String())
	}
}

func TestClientsHandler_Update_NicknameSuccess(t *testing.T) {
	svc, err := clients.NewService(t.TempDir())
	if err != nil {
		t.Fatalf("new clients service: %v", err)
	}
	if _, err := svc.Register("device-2", "profile-b", "Apple TV", "tvOS", "1.0", "AppleTV", ""); err != nil {
		t.Fatalf("seed register: %v", err)
	}

	ownership := &fakeClientOwnership{
		membership: map[string]map[string]bool{
			"account-b": {"profile-b": true},
		},
	}
	h := handlers.NewClientsHandler(svc, recordingClientSettings{}, ownership)

	nickname := "Home Theatre"
	body := map[string]any{"nickname": nickname}
	r := clientsAuthRequest(http.MethodPut, "/api/clients/device-2", body, map[string]string{"clientID": "device-2"}, "account-b", false)
	w := httptest.NewRecorder()
	h.Update(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var client models.Client
	if err := json.NewDecoder(w.Body).Decode(&client); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if client.Nickname != nickname {
		t.Fatalf("nickname = %q, want %q", client.Nickname, nickname)
	}
}
