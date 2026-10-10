package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"novastream/services/skipsegments"
)

func useSkipSegmentFakes(t *testing.T, baseURL string) {
	t.Helper()
	previous := skipSegmentService
	skipSegmentService = skipsegments.New(skipsegments.Options{
		IntroDBURL:      baseURL + "/intro",
		SkipDBURL:       baseURL + "/skip",
		ProviderTimeout: 2 * time.Second,
	})
	t.Cleanup(func() { skipSegmentService = previous })
}

func TestGetIntroSegmentsProviderPrecedence(t *testing.T) {
	introCalls, skipCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/intro":
			introCalls++
			if r.URL.Query().Get("episode") == "2" {
				_, _ = w.Write([]byte(`{"intro":{"start_ms":40000,"end_ms":90000},"recap":{"start_ms":0,"end_ms":30000},"outro":{"start_ms":1100000,"end_ms":1200000}}`))
				return
			}
			_, _ = w.Write([]byte(`{"intro":{"start_ms":40000,"end_ms":90000},"recap":null,"outro":null}`))
		case "/skip":
			skipCalls++
			if r.URL.Query().Get("duration") != "1200" {
				t.Errorf("SkipDB duration = %q, want 1200", r.URL.Query().Get("duration"))
			}
			if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Error("public SkipDB read must not send a credential")
			}
			_, _ = w.Write([]byte(`{"segments":{"intro":{"start_ms":45000,"end_ms":95000,"match":"exact"},"recap":{"start_ms":0,"end_ms":35000,"match":"shifted"},"outro":{"start_ms":1100000,"end_ms":1200000,"match":"out-of-range"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useSkipSegmentFakes(t, server.URL)

	request := httptest.NewRequest(http.MethodGet, "/video/segments?imdbId=tt987654321&season=1&episode=1&duration=1200", nil)
	response := httptest.NewRecorder()
	(&VideoHandler{}).GetIntroSegments(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var segments introDBSegmentsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &segments); err != nil {
		t.Fatal(err)
	}
	if segments.Intro == nil || *segments.Intro.StartMS != 40000 || segments.Intro.Source != "" {
		t.Errorf("IntroDB intro should win: %+v", segments.Intro)
	}
	if segments.Recap == nil || *segments.Recap.EndMS != 35000 || segments.Recap.Source != "skipdb" {
		t.Errorf("SkipDB should fill recap: %+v", segments.Recap)
	}
	if segments.Outro != nil {
		t.Errorf("out-of-range SkipDB outro should be rejected: %+v", segments.Outro)
	}
	wantMerged := []skipsegments.Segment{
		{Type: "recap", Start: 0, End: 35, Source: "skipdb"},
		{Type: "intro", Start: 40, End: 90, Source: "introdb"},
	}
	if !reflect.DeepEqual(segments.Segments, wantMerged) {
		t.Errorf("merged segments = %+v, want %+v", segments.Segments, wantMerged)
	}
	if introCalls != 1 || skipCalls != 1 {
		t.Errorf("provider calls = %d/%d, want 1/1", introCalls, skipCalls)
	}

	complete := httptest.NewRecorder()
	(&VideoHandler{}).GetIntroSegments(complete, httptest.NewRequest(http.MethodGet, "/video/segments?imdbId=tt987654321&season=1&episode=2&duration=1200", nil))
	if complete.Code != http.StatusOK || introCalls != 2 || skipCalls != 1 {
		t.Errorf("complete IntroDB result should skip fallback: status %d, provider calls %d/%d", complete.Code, introCalls, skipCalls)
	}
}

func TestGetIntroSegmentsBothProvidersFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	useSkipSegmentFakes(t, server.URL)

	response := httptest.NewRecorder()
	(&VideoHandler{}).GetIntroSegments(response, httptest.NewRequest(http.MethodGet, "/video/segments?imdbId=tt1&season=1&episode=1", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
	bad := httptest.NewRecorder()
	(&VideoHandler{}).GetIntroSegments(bad, httptest.NewRequest(http.MethodGet, "/video/segments?imdbId=nope&season=1&episode=1", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", bad.Code)
	}
}

func TestResolveSkipSegmentsPostMergesChapters(t *testing.T) {
	introCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/intro":
			introCalls++
			_, _ = w.Write([]byte(`{"intro":{"start_ms":40000,"end_ms":100000},"recap":null,"outro":null}`))
		case "/skip":
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useSkipSegmentFakes(t, server.URL)

	body := `{"imdbId":"TT42","season":1,"episode":3,"duration":1320,"chapters":[
		{"title":"Previously on Example","start":0,"end":35},
		{"title":"01 - Intro","start":35,"end":90},
		{"title":"Chapter 3","start":90,"end":1200},
		{"title":"Closing Credits","start":1200}]}`
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		(&VideoHandler{}).GetIntroSegments(response, httptest.NewRequest(http.MethodPost, "/video/segments", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var decoded resolveSkipSegmentsResponse
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		want := []skipsegments.Segment{
			{Type: "recap", Start: 0, End: 35, Source: "chapter-explicit"},
			{Type: "intro", Start: 40, End: 100, Source: "introdb"},
			{Type: "outro", Start: 1200, End: 1320, Source: "chapter-explicit"},
		}
		if !reflect.DeepEqual(decoded.Segments, want) {
			t.Fatalf("segments = %+v, want %+v", decoded.Segments, want)
		}
	}
	if introCalls != 1 {
		t.Errorf("IntroDB calls = %d, want 1 (cached)", introCalls)
	}
}

func TestResolveSkipSegmentsPostChaptersOnlyAndProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	useSkipSegmentFakes(t, server.URL)

	for _, body := range []string{
		`{"duration":100,"chapters":[{"title":"End Credits","start":85}]}`,
		`{"imdbId":"tt5","season":2,"episode":1,"duration":100,"chapters":[{"title":"End Credits","start":85}]}`,
	} {
		response := httptest.NewRecorder()
		(&VideoHandler{}).GetIntroSegments(response, httptest.NewRequest(http.MethodPost, "/video/segments", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var decoded resolveSkipSegmentsResponse
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		want := []skipsegments.Segment{{Type: "outro", Start: 85, End: 100, Source: "chapter-explicit"}}
		if !reflect.DeepEqual(decoded.Segments, want) {
			t.Fatalf("segments = %+v, want %+v", decoded.Segments, want)
		}
	}

	invalid := httptest.NewRecorder()
	(&VideoHandler{}).GetIntroSegments(invalid, httptest.NewRequest(http.MethodPost, "/video/segments", strings.NewReader("{")))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400", invalid.Code)
	}
}
