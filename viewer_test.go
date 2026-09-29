package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
