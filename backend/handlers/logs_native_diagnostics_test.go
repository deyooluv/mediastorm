package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeDiagnosticsBatchLog(id, body string) string {
	return strings.Join([]string{
		fmt.Sprintf("2026-10-08T13:38:21.000Z [INFO ] [NativeDiagnosticsPending] BEGIN id=%s capturedAt=2026-10-08T13:38:20.900Z", id),
		fmt.Sprintf("2026-10-08T13:38:21.001Z [ERROR] [NativeDiagnosticsPending] id=%s kind=crash chunk=1/1", id),
		body,
		fmt.Sprintf("2026-10-08T13:38:21.002Z [INFO ] [NativeDiagnosticsPending] END id=%s", id),
	}, "\n")
}

func uploadTestFrontendLogs(t *testing.T, h *LogsHandler, clientID, logs string) {
	t.Helper()
	body, err := json.Marshal(uploadFrontendLogsRequest{FrontendLogs: logs, DeviceType: "Apple TV", OS: "tvOS", AppVersion: "1.0.0"})
	if err != nil {
		t.Fatalf("marshal upload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/frontend", strings.NewReader(string(body)))
	req.Header.Set("X-Client-ID", clientID)
	rec := httptest.NewRecorder()
	h.UploadFrontendLogs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
	}
}

func newNativeDiagnosticsTestHandler(t *testing.T) *LogsHandler {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "backend.log")
	if err := os.WriteFile(logFile, []byte("backend line\n"), 0o644); err != nil {
		t.Fatalf("write backend log: %v", err)
	}
	return NewLogsHandler(log.New(os.Stdout, "", 0), logFile)
}

func TestExtractNativeDiagnosticBatchesKeepsOnlyCompleteBatches(t *testing.T) {
	logs := strings.Join([]string{
		"2026-10-08T13:38:20.000Z [INFO ] [NativeDiagnosticsPending] END id=cut-start",
		"app line",
		nativeDiagnosticsBatchLog("first", "=== STRMR NATIVE CRASH (signal 5)\nframe 0 FFmpegDecode"),
		"app line",
		nativeDiagnosticsBatchLog("second", "[MainWatchdog] responsive availableMB=120"),
		"2026-10-08T13:38:22.000Z [INFO ] [NativeDiagnosticsPending] BEGIN id=cut-end capturedAt=2026-10-08T13:38:22Z",
		"partial ring",
	}, "\n")

	batches := extractNativeDiagnosticBatches(logs)
	if len(batches) != 2 {
		t.Fatalf("extracted %d batches, want 2: %#v", len(batches), batches)
	}
	if batches[0].ID != "first" || batches[0].CapturedAt != "2026-10-08T13:38:20.900Z" {
		t.Fatalf("first batch = %#v", batches[0])
	}
	if !strings.Contains(batches[0].Text, "frame 0 FFmpegDecode") || strings.Contains(batches[0].Text, "app line") {
		t.Fatalf("first batch text = %q", batches[0].Text)
	}
	if batches[1].ID != "second" || !strings.Contains(batches[1].Text, "availableMB=120") {
		t.Fatalf("second batch = %#v", batches[1])
	}
}

func TestLogsHandler_RetainsNativeDiagnosticsAcrossUploads(t *testing.T) {
	h := newNativeDiagnosticsTestHandler(t)

	crash := nativeDiagnosticsBatchLog("crash1", "=== STRMR NATIVE CRASH (signal 5)")
	uploadTestFrontendLogs(t, h, "atv-1", "relaunch line\n"+crash)
	// The same batch rides along in later uploads until it leaves the app's log tail.
	uploadTestFrontendLogs(t, h, "atv-1", crash+"\nhome line")
	// Leaving the player replaces the snapshot with logs that no longer hold the batch.
	uploadTestFrontendLogs(t, h, "atv-1", "player exit line")

	snapshot, err := h.GetFrontendLogSnapshot("atv-1")
	if err != nil {
		t.Fatalf("GetFrontendLogSnapshot() error = %v", err)
	}
	if strings.Contains(snapshot.FrontendLogs, "STRMR NATIVE CRASH") {
		t.Fatalf("latest snapshot unexpectedly still holds the crash batch")
	}

	summaries, err := h.ListFrontendLogSummaries()
	if err != nil {
		t.Fatalf("ListFrontendLogSummaries() error = %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %#v, want only the snapshot (retained diagnostics are not snapshots)", summaries)
	}

	frontendLogs, summaries, err := h.readAggregatedFrontendLogs(maxLogLines, "*")
	if err != nil {
		t.Fatalf("readAggregatedFrontendLogs() error = %v", err)
	}
	pkg := h.buildCombinedStoredLogsPackage(frontendLogs, summaries)
	if !strings.Contains(pkg, "NATIVE CRASH DIAGNOSTICS") || !strings.Contains(pkg, "=== STRMR NATIVE CRASH (signal 5)") {
		t.Fatalf("package is missing the retained crash batch:\n%s", pkg)
	}
	if strings.Count(pkg, "batch crash1 ") != 1 {
		t.Fatalf("crash batch retained %d times, want 1", strings.Count(pkg, "batch crash1 "))
	}
	if strings.Index(pkg, "NATIVE CRASH DIAGNOSTICS") > strings.Index(pkg, "BACKEND LOGS") {
		t.Fatalf("native diagnostics should precede the line-limited log sections")
	}
}

func TestLogsHandler_RetainsNewestNativeDiagnosticBatches(t *testing.T) {
	h := newNativeDiagnosticsTestHandler(t)
	for i := 1; i <= maxRetainedNativeDiagnosticBatches+2; i++ {
		uploadTestFrontendLogs(t, h, "atv-1", nativeDiagnosticsBatchLog(fmt.Sprintf("crash%d", i), "ring"))
	}

	records := h.readNativeDiagnosticsRecords([]string{"atv-1"})
	if len(records) != 1 || len(records[0].Batches) != maxRetainedNativeDiagnosticBatches {
		t.Fatalf("records = %#v, want %d batches", records, maxRetainedNativeDiagnosticBatches)
	}
	if records[0].Batches[0].ID != "crash3" || records[0].Batches[len(records[0].Batches)-1].ID != fmt.Sprintf("crash%d", maxRetainedNativeDiagnosticBatches+2) {
		t.Fatalf("retained batches = %#v", records[0].Batches)
	}
}

func TestLogsHandler_UnreadableNativeDiagnosticsRecordIsReplaced(t *testing.T) {
	h := newNativeDiagnosticsTestHandler(t)
	path := h.nativeDiagnosticsPath("atv-1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}

	uploadTestFrontendLogs(t, h, "atv-1", nativeDiagnosticsBatchLog("crash1", "ring"))

	records := h.readNativeDiagnosticsRecords([]string{"atv-1"})
	if len(records) != 1 || len(records[0].Batches) != 1 || records[0].Batches[0].ID != "crash1" {
		t.Fatalf("records = %#v", records)
	}
}

func TestClipNativeDiagnosticTextKeepsBothEnds(t *testing.T) {
	text := "CRASH-HEADER" + strings.Repeat("x", 5_000) + "LAST-NATIVE-OP"
	clipped := clipNativeDiagnosticText(text, 1_000)
	if len(clipped) > 1_000 {
		t.Fatalf("clipped length = %d, want <= 1000", len(clipped))
	}
	if !strings.HasPrefix(clipped, "CRASH-HEADER") || !strings.HasSuffix(clipped, "LAST-NATIVE-OP") || !strings.Contains(clipped, "bytes clipped") {
		t.Fatalf("clipped text lost an end: %q", clipped)
	}
	if got := clipNativeDiagnosticText("short", 1_000); got != "short" {
		t.Fatalf("short text changed to %q", got)
	}
}

func TestLogsHandler_UploadFailsWhenNativeDiagnosticsCannotBeRetained(t *testing.T) {
	h := newNativeDiagnosticsTestHandler(t)
	// A file where the retention directory belongs makes the write fail.
	if err := os.MkdirAll(h.frontendLogsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.frontendLogsDir, nativeDiagnosticsDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	body, _ := json.Marshal(uploadFrontendLogsRequest{FrontendLogs: nativeDiagnosticsBatchLog("crash1", "ring")})
	req := httptest.NewRequest(http.MethodPost, "/api/logs/frontend", strings.NewReader(string(body)))
	req.Header.Set("X-Client-ID", "atv-1")
	rec := httptest.NewRecorder()
	h.UploadFrontendLogs(rec, req)

	// A failed upload keeps the batch pending on the device for the next upload.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("upload status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}
