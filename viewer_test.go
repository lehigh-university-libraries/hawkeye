package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestViewerReadOnlyLatestResults(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Rodale")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "image.png")
	path := filepath.Join(dir, "report.json")
	data := reportBytes(t,
		result{File: file, Page: 1, PageCount: 1, Status: "error", Error: "old error"},
		result{File: file, Page: 1, PageCount: 1, Status: "no_findings", Text: "private OCR text"},
		result{File: filepath.Join(dir, "<script>alert(1)</script>.png"), Page: 1, PageCount: 1, Status: "review", Findings: []finding{{Kind: "bank_account"}}},
	)
	data = append(data, `{"file":"unfinished`...)
	if err := os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	handler, err := reportHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := findReports(root)
	if err != nil || len(paths) != 1 {
		t.Fatalf("discovered %d reports: %v", len(paths), err)
	}
	var id string
	for key := range paths {
		id = key
	}
	v := viewReport(root, id, path, false)
	if v.Errors != 0 || v.Review != 1 || v.Pages != 2 || v.CompleteFiles != 2 || !v.Partial || len(v.Rows) != 1 {
		t.Fatalf("incorrect latest results: %+v", v)
	}
	for _, target := range []string{"/", "/report?id=" + id, "/report?id=" + id + "&all=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		body := response.Body.String()
		if response.Code != 200 || !strings.Contains(body, "Rodale") || strings.Contains(body, "private OCR text") || strings.Contains(body, "old error") || strings.Contains(body, "<script>alert") {
			t.Fatalf("unsafe or incorrect response for %s: %d", target, response.Code)
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing privacy/security headers")
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/report?id="+id, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("accepted %s", method)
		}
	}
	for _, target := range []string{"/report?id=../../etc/passwd", "/etc/passwd", "/assets/../report.json"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code == 200 {
			t.Fatalf("served arbitrary path %s", target)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, got) {
		t.Fatal("viewer modified report or repaired its partial tail")
	}
}

func TestViewerDiscoveryAndSpreadsheetCells(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "report.jsonl")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "report.jsonl")); err != nil {
		t.Fatal(err)
	}
	paths, err := findReports(root)
	if err != nil || len(paths) != 0 {
		t.Fatal("followed a report symlink")
	}
	for _, value := range []string{"=IMPORTXML(1)", "+1", "-1", "@SUM(A1)", "\t=1"} {
		if !strings.HasPrefix(sheetCell(value), "'") {
			t.Fatal("spreadsheet formula was not escaped")
		}
	}
}

func TestViewerFailureModes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "report.jsonl")
	records := []result{
		{File: filepath.Join(root, "retry.pdf"), Page: 1, PageCount: 2, Status: "retrying", RequestRetries: 1, RetryStage: "OCR", RetryAttempt: 1, RetryAt: time.Date(2026, 9, 30, 12, 0, 5, 0, time.UTC), Error: "Ollama HTTP 500"},
		{File: filepath.Join(root, "stopped.pdf"), Page: 3, PageCount: 5, Status: "error", DocumentStopped: true, RequestRetries: 5},
		{File: filepath.Join(root, "repeated.pdf"), Page: 1, PageCount: 1, Status: "error", RetryCount: 2, Error: "<script>private()</script>"},
		{File: filepath.Join(root, "dead.pdf"), Page: 1, PageCount: 1, Status: "error", RetryCount: 11, DeadLetter: true},
		{File: filepath.Join(root, "recovered.pdf"), Page: 1, PageCount: 1, Status: "no_findings", RequestRetries: 1, Text: "private OCR text"},
		{File: filepath.Join(root, "upstream.pdf"), Page: 3, PageCount: 5, Status: "error", DocumentStopped: true, CircuitOpen: true, RequestRetries: 5},
	}
	if err := os.WriteFile(path, reportBytes(t, records...), 0600); err != nil {
		t.Fatal(err)
	}
	v := viewReport(root, "test", path, false)
	if !v.CircuitOpen || v.Retrying != 1 || v.StoppedDocuments != 2 || v.PersistentFiles != 2 || v.DeadLetters != 1 || v.CompleteFiles != 1 || v.Pages != 1 || v.Errors != 4 || len(v.Rows) != 5 {
		t.Fatalf("incorrect failure state: %+v", v)
	}
	handler, err := reportHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := findReports(root)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for key := range paths {
		id = key
	}
	for _, target := range []string{"/", "/report?id=" + id, "/report?id=" + id + "&all=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		body := response.Body.String()
		if response.Code != 200 || strings.Contains(body, "<script>private()") || strings.Contains(body, "private OCR text") {
			t.Fatalf("unsafe or failed response: %s", body)
		}
		if target == "/" {
			if !strings.Contains(body, "Possible upstream issue") || !strings.Contains(body, "dead letters") {
				t.Fatalf("missing collection failure summary: %s", body)
			}
			continue
		}
		for _, want := range []string{"Request retries", "Document stops", "Upstream check", "Persistent failures", "Retrying request", "Document stopped", "Repeated failure — investigate", "Dead letter — investigate", "Failed retry passes", "2026-09-30 12:00:05 UTC"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.HasSuffix(target, "all=1") && !strings.Contains(body, "Recovered after retries") {
			t.Error("missing successful recovery state")
		}
	}
	// A later record from the stopped document clears its last saved stop; any
	// later progress also clears the report's last circuit-break outcome.
	records = append(records, result{File: records[5].File, Page: 1, PageCount: 5, Status: "no_findings"})
	if err := os.WriteFile(path, reportBytes(t, records...), 0600); err != nil {
		t.Fatal(err)
	}
	v = viewReport(root, "test", path, false)
	if v.CircuitOpen || v.StoppedDocuments != 1 {
		t.Fatalf("stale stop state: %+v", v)
	}
}

func TestViewerRetainsDeadLettersAfterEnumerationFailure(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "doc.pdf")
	path := filepath.Join(root, "report.jsonl")
	data := reportBytes(t, result{File: file, Page: 1, PageCount: 2, Status: "error", RetryCount: 11, DeadLetter: true}, result{File: file, Status: "error", Error: "enumeration failed"})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	v := viewReport(root, "test", path, false)
	if v.DeadLetters != 1 || v.CompleteFiles != 0 || len(v.Rows) != 2 {
		t.Fatalf("lost dead letter: %+v", v)
	}
}
