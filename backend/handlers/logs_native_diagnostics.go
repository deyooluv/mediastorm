package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Native crash reports and playback breadcrumbs reach the backend once: the app
// logs them as a [NativeDiagnosticsPending] batch on the relaunch after a crash
// and deletes its copy after the first upload that contains the whole batch.
// Frontend snapshots are replaced on every upload, so the batch is retained
// separately here and survives later uploads from the same device.
const (
	nativeDiagnosticsDirName           = "native_diagnostics"
	maxRetainedNativeDiagnosticBatches = 5
	maxNativeDiagnosticBatchBytes      = 1 << 20
)

var (
	nativeDiagnosticsBeginPattern = regexp.MustCompile(`\[NativeDiagnosticsPending\] BEGIN id=([A-Za-z0-9_-]+)(?: capturedAt=(\S+))?`)
	nativeDiagnosticsEndPattern   = regexp.MustCompile(`\[NativeDiagnosticsPending\] END id=([A-Za-z0-9_-]+)`)
)

type nativeDiagnosticsBatch struct {
	ID         string    `json:"id"`
	CapturedAt string    `json:"capturedAt,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`
	AppVersion string    `json:"appVersion,omitempty"`
	Text       string    `json:"text"`
}

type nativeDiagnosticsRecord struct {
	ClientID   string                   `json:"clientId"`
	DeviceType string                   `json:"deviceType,omitempty"`
	OS         string                   `json:"os,omitempty"`
	Batches    []nativeDiagnosticsBatch `json:"batches"`
}

// extractNativeDiagnosticBatches returns every complete BEGIN…END batch in an
// uploaded log. A batch cut off by the upload's line limit is skipped; the app
// keeps it pending and logs it again on the next launch.
func extractNativeDiagnosticBatches(logs string) []nativeDiagnosticsBatch {
	var batches []nativeDiagnosticsBatch
	var current *nativeDiagnosticsBatch
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if match := nativeDiagnosticsBeginPattern.FindStringSubmatch(line); match != nil {
			current = &nativeDiagnosticsBatch{ID: match[1], CapturedAt: match[2]}
			lines = []string{line}
			continue
		}
		if current == nil {
			continue
		}
		lines = append(lines, line)
		if match := nativeDiagnosticsEndPattern.FindStringSubmatch(line); match != nil && match[1] == current.ID {
			current.Text = clipNativeDiagnosticText(strings.Join(lines, "\n"), maxNativeDiagnosticBatchBytes)
			batches = append(batches, *current)
			current = nil
			lines = nil
		}
	}
	return batches
}

// clipNativeDiagnosticText bounds an oversized batch while keeping both ends:
// the crash header and stack lead the batch, the final native operations end it.
func clipNativeDiagnosticText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	marker := fmt.Sprintf("\n[... %d bytes clipped ...]\n", len(text)-limit)
	half := (limit - len(marker)) / 2
	return text[:half] + marker + text[len(text)-half:]
}

// mergeNativeDiagnosticBatches appends unseen batches and keeps the newest limit.
func mergeNativeDiagnosticBatches(existing, incoming []nativeDiagnosticsBatch, limit int) []nativeDiagnosticsBatch {
	seen := make(map[string]struct{}, len(existing))
	merged := append([]nativeDiagnosticsBatch(nil), existing...)
	for _, batch := range existing {
		seen[batch.ID] = struct{}{}
	}
	for _, batch := range incoming {
		if _, ok := seen[batch.ID]; ok {
			continue
		}
		seen[batch.ID] = struct{}{}
		merged = append(merged, batch)
	}
	if len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	return merged
}

func (h *LogsHandler) nativeDiagnosticsPath(clientID string) string {
	return filepath.Join(h.frontendLogsDir, nativeDiagnosticsDirName, urlSafeFileName(clientID)+".json")
}

// retainNativeDiagnostics stores the complete native diagnostic batches found in
// an uploaded snapshot. It reports whether any new batch was stored.
func (h *LogsHandler) retainNativeDiagnostics(snapshot frontendLogSnapshot) (bool, error) {
	incoming := extractNativeDiagnosticBatches(snapshot.FrontendLogs)
	if len(incoming) == 0 {
		return false, nil
	}
	for i := range incoming {
		incoming[i].ReceivedAt = snapshot.UploadedAt
		incoming[i].AppVersion = snapshot.AppVersion
	}

	h.frontendLogsMu.Lock()
	defer h.frontendLogsMu.Unlock()

	path := h.nativeDiagnosticsPath(snapshot.ClientID)
	record, err := readNativeDiagnosticsRecord(path)
	if err != nil && !os.IsNotExist(err) {
		// An unreadable record must not block every future upload from this
		// device; start a fresh record so the incoming batch is still kept.
		h.logger.Printf("[logs] Replacing unreadable native diagnostics record for client %s: %v", truncateLogIdentifier(snapshot.ClientID), err)
	}
	if record == nil {
		record = &nativeDiagnosticsRecord{}
	}
	// Every upload made while a batch is still in the app's log tail repeats it,
	// so most uploads carry batches that are already stored.
	stored := make(map[string]struct{}, len(record.Batches))
	for _, batch := range record.Batches {
		stored[batch.ID] = struct{}{}
	}
	hasNew := false
	for _, batch := range incoming {
		if _, ok := stored[batch.ID]; !ok {
			hasNew = true
			break
		}
	}
	if !hasNew {
		return false, nil
	}
	record.ClientID = snapshot.ClientID
	record.DeviceType = snapshot.DeviceType
	record.OS = snapshot.OS
	record.Batches = mergeNativeDiagnosticBatches(record.Batches, incoming, maxRetainedNativeDiagnosticBatches)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, data, 0o644)
}

func readNativeDiagnosticsRecord(path string) (*nativeDiagnosticsRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record nativeDiagnosticsRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (h *LogsHandler) readNativeDiagnosticsRecords(clientIDs []string) []nativeDiagnosticsRecord {
	h.frontendLogsMu.RLock()
	defer h.frontendLogsMu.RUnlock()

	records := make([]nativeDiagnosticsRecord, 0, len(clientIDs))
	for _, clientID := range clientIDs {
		record, err := readNativeDiagnosticsRecord(h.nativeDiagnosticsPath(clientID))
		if err != nil || len(record.Batches) == 0 {
			continue
		}
		records = append(records, *record)
	}
	return records
}

// writeNativeDiagnosticsSection renders retained batches for the given clients.
// It writes nothing when no client has retained native diagnostics.
func (h *LogsHandler) writeNativeDiagnosticsSection(combined *strings.Builder, clientIDs []string) {
	records := h.readNativeDiagnosticsRecords(clientIDs)
	if len(records) == 0 {
		return
	}

	combined.WriteString("═══════════════════════════════════════════════════════════════════════\n")
	combined.WriteString("                    NATIVE CRASH DIAGNOSTICS\n")
	combined.WriteString(fmt.Sprintf("      (newest %d per client, retained across log uploads)\n", maxRetainedNativeDiagnosticBatches))
	combined.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
	for _, record := range records {
		for _, batch := range record.Batches {
			combined.WriteString(fmt.Sprintf("--- client %s %s %s app %s | batch %s captured %s received %s\n",
				record.ClientID,
				strings.TrimSpace(record.DeviceType),
				strings.TrimSpace(record.OS),
				strings.TrimSpace(batch.AppVersion),
				batch.ID,
				batch.CapturedAt,
				batch.ReceivedAt.UTC().Format(time.RFC3339),
			))
			combined.WriteString(batch.Text)
			combined.WriteString("\n\n")
		}
	}
}
