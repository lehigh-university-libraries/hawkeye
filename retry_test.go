package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOllamaBackoff(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		status, failures, limit, wantCalls int
		stage                              string
		wantError                          bool
	}{
		{"500 recovers", 500, 2, 0, 3, "OCR", false},
		{"network disconnect recovers", 0, 2, 0, 3, "OCR", false},
		{"invalid response recovers", 200, 1, 0, 2, "OCR", false},
		{"exhausted", 500, 99, 0, 6, "OCR", true},
		{"assessment retries", 500, 1, 0, 2, "Assessment", false},
		{"permanent error", 401, 99, 0, 1, "OCR", true},
		{"reserve assessment allowance", 500, 99, 3, 2, "OCR", true},
		{"assessment allowance", 500, 99, 3, 3, "Assessment", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if int(calls.Add(1)) <= tc.failures {
					if tc.status == 0 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, "secret response content")
					return
				}
				_, _ = io.WriteString(w, `{"response":"recovered"}`)
			}))
			defer server.Close()
			t.Setenv("OLLAMA_URL", server.URL)
			var progress bytes.Buffer
			requests := 0
			o := options{timeout: time.Second, retryDelays: make([]time.Duration, 5), progress: &progress, requests: &requests, limit: tc.limit}
			text, err := o.extract(context.Background(), base64.StdEncoding.EncodeToString(testPNG(t)), tc.stage, "test", "test", nil)
			if (err != nil) != tc.wantError || int(calls.Load()) != tc.wantCalls || requests != tc.wantCalls {
				t.Fatalf("calls=%d requests=%d text=%q err=%v", calls.Load(), requests, text, err)
			}
			if !tc.wantError && text != "recovered" {
				t.Fatalf("text=%q", text)
			}
			if strings.Contains(progress.String(), "secret response content") {
				t.Fatal("response leaked in retry log")
			}
			if tc.wantCalls > 1 && !strings.Contains(progress.String(), fmt.Sprintf("retry %d/5", tc.wantCalls-1)) {
				t.Fatalf("missing retry progress: %s", &progress)
			}
		})
	}
}

func TestCancelOllamaBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer server.Close()
	t.Setenv("OLLAMA_URL", server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var progress bytes.Buffer
	o := options{timeout: time.Second, progress: cancelOnRetry{&progress, cancel}}
	_, err := o.extract(ctx, base64.StdEncoding.EncodeToString(testPNG(t)), "OCR", "test", "test", nil)
	if err != context.Canceled || calls.Load() != 1 || !strings.Contains(progress.String(), "retry 1/5 in 5s") {
		t.Fatalf("calls=%d err=%v progress=%s", calls.Load(), err, &progress)
	}
}

type cancelOnRetry struct {
	io.Writer
	cancel context.CancelFunc
}

func (w cancelOnRetry) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if bytes.Contains(p, []byte("retry 1/5")) {
		w.cancel()
	}
	return n, err
}

// Exercise the scanner with a local Ollama stub and a Docker stand-in that
// enumerates/renders synthetic pages, without needing a running Docker daemon.
func retryScanSetup(t *testing.T, count, documents int) (string, options, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	bin := t.TempDir()
	png := filepath.Join(bin, "page.png")
	if err := os.WriteFile(png, testPNG(t), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cat >/dev/null
case "$*" in
 *'magick identify'*) i=0; while [ "$i" -lt "$HAWKEYE_TEST_PAGES" ]; do echo "$HAWKEYE_TEST_PAGES"; i=$((i+1)); done ;;
 *) cat "$HAWKEYE_TEST_PNG" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HAWKEYE_TEST_PAGES", fmt.Sprint(count))
	t.Setenv("HAWKEYE_TEST_PNG", png)
	for i := 0; i < documents; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.pdf", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	success, calls := new(atomic.Bool), new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			_, _ = io.WriteString(w, `{"capabilities":["vision"]}`)
			return
		}
		calls.Add(1)
		if !success.Load() {
			http.Error(w, "private document content", 500)
			return
		}
		var request struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		response := "A garden."
		if request.Model == "assessment" {
			response = negativeAssessment
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"response": response})
	}))
	t.Cleanup(server.Close)
	t.Setenv("OLLAMA_URL", server.URL)
	o := options{depth: -1, dpi: 200, timeout: time.Second, image: "test", model: "ocr", analysisModel: "assessment", endpoint: server.URL, output: filepath.Join(dir, "report.jsonl"), retryDelays: make([]time.Duration, 5)}
	return dir, o, success, calls
}

func scanRecords(t *testing.T, path string) []result {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []result
	if _, err := readReport(bytes.NewReader(data), func(r result) {
		if r.Status != "retrying" {
			records = append(records, r)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestDocumentCircuitBreaker(t *testing.T) {
	for _, count := range []int{4} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			dir, o, _, calls := retryScanSetup(t, count, 4)
			var progress bytes.Buffer
			err := run(context.Background(), dir, o, io.Discard, &progress)
			if err == nil || !strings.Contains(err.Error(), "circuit breaker") {
				t.Fatalf("err=%v progress=%s", err, &progress)
			}
			records := scanRecords(t, o.output)
			wantPages := min(count, 3) * 3
			if len(records) != wantPages || int(calls.Load()) != wantPages*6 {
				t.Fatalf("records=%d calls=%d", len(records), calls.Load())
			}
			v := viewReport(dir, "test", o.output, false)
			if !v.CircuitOpen || v.StoppedDocuments != 3 {
				t.Fatalf("missing durable circuit state: %+v", v)
			}
			for _, r := range records {
				if r.RequestRetries != 5 || r.DocumentStopped != (r.Page == 3) {
					t.Fatalf("missing durable retry/document state: %+v", r)
				}
				if filepath.Base(r.File) == "3.pdf" || r.Page > 3 || r.RetryCount != 0 || r.DeadLetter {
					t.Fatalf("unexpected record: %+v", r)
				}
			}
		})
	}
}

func TestResumeRetriesOldErrorsFirstAndDeadLetters(t *testing.T) {
	dir, o, _, calls := retryScanSetup(t, 4, 2)
	old := result{File: filepath.Join(dir, "1.pdf"), Page: 1, PageCount: 4, Status: "error", RetryCount: 10}
	if err := os.WriteFile(o.output, reportBytes(t, old), 0600); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	err := run(context.Background(), dir, o, io.Discard, &progress)
	if err == nil || strings.Contains(err.Error(), "circuit breaker") {
		t.Fatalf("old failure counted toward circuit: %v", err)
	}
	records := scanRecords(t, o.output)
	if len(records) != 8 || records[1].File != old.File || records[1].Page != 1 || records[1].RetryCount != 11 || !records[1].DeadLetter {
		t.Fatalf("retry ordering/dead letter incorrect: %+v", records)
	}
	if calls.Load() != 42 {
		t.Fatalf("calls=%d", calls.Load())
	}
	calls.Store(0)
	progress.Reset()
	o.retryErrors = true // The explicit flag has the same behavior as normal resume.
	if err := run(context.Background(), dir, o, io.Discard, &progress); err == nil || strings.Contains(err.Error(), "circuit breaker") {
		t.Fatalf("err=%v", err)
	}
	all := scanRecords(t, o.output)
	for _, r := range all[len(records):] {
		if r.File == old.File && r.Page == 1 {
			t.Fatal("dead letter retried")
		}
	}
	// Six old failures each receive a full backoff pass, then the previously
	// skipped fourth page of the first document gets its initial six attempts.
	if calls.Load() != 42 {
		t.Fatalf("resume calls=%d", calls.Load())
	}
}

func TestOldFailuresDoNotTripCircuit(t *testing.T) {
	dir, o, _, _ := retryScanSetup(t, 4, 4)
	var old []result
	for doc := 0; doc < 4; doc++ {
		for page := 1; page <= 4; page++ {
			old = append(old, result{File: filepath.Join(dir, fmt.Sprintf("%d.pdf", doc)), Page: page, PageCount: 4, Status: "error"})
		}
	}
	if err := os.WriteFile(o.output, reportBytes(t, old...), 0600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), dir, o, io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), "circuit breaker") {
		t.Fatalf("err=%v", err)
	}
	if got := len(scanRecords(t, o.output)); got != 32 {
		t.Fatalf("records=%d", got)
	}
}

func TestDeadLetterCountsPasses(t *testing.T) {
	dir, o, _, calls := retryScanSetup(t, 1, 1)
	old := result{File: filepath.Join(dir, "0.pdf"), Page: 1, PageCount: 1, Status: "error", RetryCount: 9}
	if err := os.WriteFile(o.output, reportBytes(t, old), 0600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{10, 11} {
		if err := run(context.Background(), dir, o, io.Discard, io.Discard); err == nil {
			t.Fatal("expected failed pass")
		}
		records := scanRecords(t, o.output)
		last := records[len(records)-1]
		if last.RetryCount != want || last.DeadLetter != (want == 11) {
			t.Fatalf("result=%+v", last)
		}
	}
	if calls.Load() != 12 {
		t.Fatalf("each retry pass should make six requests: %d", calls.Load())
	}
	if err := run(context.Background(), dir, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 12 || len(scanRecords(t, o.output)) != 3 {
		t.Fatal("dead letter was retried")
	}
}

func TestSuccessfulDocumentResetsCircuit(t *testing.T) {
	dir, o, success, _ := retryScanSetup(t, 4, 7)
	progress := callbackWriter(func(p []byte) {
		if bytes.HasPrefix(p, []byte("Document ")) {
			success.Store(bytes.HasPrefix(p, []byte("Document 3/7:")))
		}
	})
	err := run(context.Background(), dir, o, io.Discard, progress)
	if err == nil || !strings.Contains(err.Error(), "circuit breaker") {
		t.Fatalf("err=%v", err)
	}
	records := scanRecords(t, o.output)
	// Two failing documents, a successful document, then three failing documents.
	if len(records) != 19 || filepath.Base(records[len(records)-1].File) != "5.pdf" {
		t.Fatalf("records=%+v", records)
	}
}

type callbackWriter func([]byte)

func (w callbackWriter) Write(p []byte) (int, error) { w(p); return len(p), nil }

func TestRetryErrorsRequiresReport(t *testing.T) {
	dir, o, _, calls := retryScanSetup(t, 1, 1)
	o.retryErrors = true
	for _, output := range []string{o.output, "-"} {
		o.output = output
		err := run(context.Background(), dir, o, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "requires an existing output report") {
			t.Fatalf("err=%v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("requests sent without an existing report")
	}
	cmd := newCommand()
	if err := cmd.ParseFlags([]string{"--retry-errors"}); err != nil {
		t.Fatal(err)
	}
	if enabled, err := cmd.Flags().GetBool("retry-errors"); err != nil || !enabled {
		t.Fatal("retry-errors flag unavailable")
	}
}

func TestRetryProgressIsVisibleAndResumable(t *testing.T) {
	dir, o, success, _ := retryScanSetup(t, 1, 1)
	starts := 0
	progress := callbackWriter(func(p []byte) {
		if !bytes.Contains(p, []byte("OCR (ocr)...")) {
			return
		}
		starts++
		if starts != 2 {
			return
		}
		v := viewReport(dir, "test", o.output, false)
		if v.Retrying != 1 || v.Errors != 0 || v.CompleteFiles != 0 || len(v.Rows) != 1 || v.Rows[0].State != "Retrying request" || v.Rows[0].RetryCount != 0 {
			t.Errorf("retry progress missing: %+v", v)
		}
		c, err := loadReport(o.output)
		if err != nil {
			t.Fatal(err)
		}
		defer c.file.Close()
		file := filepath.Join(dir, "0.pdf")
		if c.pageDone(file, 1) || !c.pageFailed(file, 1) || c.pageState(file, 1).retries != 0 {
			t.Error("unfinished request marked complete or counted as a failed pass")
		}
		success.Store(true)
	})
	if err := run(context.Background(), dir, o, io.Discard, progress); err != nil {
		t.Fatal(err)
	}
	records := scanRecords(t, o.output)
	if starts != 2 || len(records) != 1 || records[0].RequestRetries != 1 || records[0].Status != "no_findings" {
		t.Fatalf("missing recovered result: %+v", records)
	}
	v := viewReport(dir, "test", o.output, true)
	if v.Retrying != 0 || v.Errors != 0 || v.CompleteFiles != 1 || v.Rows[0].State != "Recovered after retries" {
		t.Fatalf("stale retry progress: %+v", v)
	}
}

func TestRetryProgressSaveFailureStopsRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	t.Setenv("OLLAMA_URL", server.URL)
	saveErr := fmt.Errorf("report write failed")
	o := options{timeout: time.Second, retryDelays: make([]time.Duration, 5), onRetry: func(string, int, time.Duration, error) error { return saveErr }}
	_, err := o.extract(context.Background(), base64.StdEncoding.EncodeToString(testPNG(t)), "OCR", "test", "test", nil)
	if err != saveErr || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
