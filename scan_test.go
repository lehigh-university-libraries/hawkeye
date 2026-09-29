package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lehigh-university-libraries/htr/pkg/providers"
)

func TestReportCheckpointSurvivesKill(t *testing.T) {
	if path := os.Getenv("HAWKEYE_TEST_CHECKPOINT_PATH"); path != "" {
		// The parent kills this process without allowing Close or any deferred
		// cleanup. Use the same per-record save path as the scanner.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r := result{File: "synthetic.pdf", Page: 1, Status: "review", Text: "synthetic OCR", Findings: []finding{}}
		if err := saveResult(json.NewEncoder(f), f, r); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "saved")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReportCheckpointSurvivesKill$")
	cmd.Env = append(os.Environ(), "HAWKEYE_TEST_CHECKPOINT_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "saved\n" {
		t.Fatalf("child failed to checkpoint: %q, %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected killed child")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("checkpoint is not valid JSON: %v", err)
	}
	if got.Page != 1 || got.Status != "review" || got.Text != "synthetic OCR" || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("checkpoint lost after hard kill: %+v", got)
	}
}

func TestReportCheckpointErrors(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "report-")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := saveResult(json.NewEncoder(f), f, result{}); err == nil || !strings.HasPrefix(err.Error(), "write report:") {
		t.Fatalf("write failure was not propagated: %v", err)
	}
	if err := saveResult(json.NewEncoder(io.Discard), f, result{}); err == nil || !strings.HasPrefix(err.Error(), "sync report:") {
		t.Fatalf("sync failure was not propagated: %v", err)
	}
}

func TestDiscovery(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.PDF", "ignore.txt", "child/b.tiff", "child/grand/c.JP2"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "a.PDF"), filepath.Join(root, "link.pdf")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ depth, want int }{{0, 1}, {1, 2}, {9, 3}, {-1, 3}} {
		got, err := discover(root, tc.depth)
		if err != nil || len(got) != tc.want {
			t.Fatalf("depth %d: %v %v", tc.depth, got, err)
		}
	}
	files, err := discover(filepath.Join(root, "a.PDF"), 0)
	if err != nil || len(files) != 1 {
		t.Fatalf("single input: %v %v", files, err)
	}
}

func TestDetection(t *testing.T) {
	for _, tc := range []struct{ text, kind string }{
		{"Account number:\n12345678", "bank_account"},
		{"Acct # 12345678", "bank_account"},
		{"Account 1234", "bank_account"},
		{"An account in Merchants Bank was opened on\nDeposits to this account have totaled\nThe current balance in the account as of July 31, 1990 is", ""},
		{"Account opened in 1990", ""},
		{"⑆021000021⑆ 123456789⑈", "bank_number_candidate"},
		{"Routing: 021000021", "routing_number"},
		{"SSN 123-45-6789", "ssn"},
		{"Card 4111 1111 1111 1111", "credit_card"},
		{"Call 610-555-0123. Address: 123 Main Street.", ""},
		{"000-00-0000 999-99-9999 0000 0000 0000 0000", ""},
	} {
		got := detect(tc.text)
		if tc.kind == "" {
			if len(got) != 0 {
				t.Errorf("unexpected findings: %v", got)
			}
			continue
		}
		found := false
		for _, f := range got {
			if f.Kind == tc.kind {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s in %v", tc.kind, got)
		}
	}
}

const negativeAssessment = `{"contains_sensitive":false,"readable":true,"sensitive_likelihood_percent":0,"kinds":[]}`

func TestAssessment(t *testing.T) {
	valid := `{"contains_sensitive":true,"readable":true,"sensitive_likelihood_percent":85,"kinds":["bank_account"]}`
	if _, err := parseAssessment(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `null`, "not JSON", `{"text":"A garden."}`, strings.Replace(valid, "85", "101", 1), strings.Replace(valid, "true", "false", 1), strings.Replace(valid, "bank_account", "123456789", 1), strings.Replace(valid, `["bank_account"]`, `[]`, 1)} {
		if _, err := parseAssessment(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := parseAssessment("json\n" + valid); err != nil {
		t.Fatal(err)
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	img.Set(0, 0, color.White)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImageService(t *testing.T) {
	data := testPNG(t)
	input := []byte("synthetic TIFF bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Match Houdini's single destination MIME type contract. A comma-separated
		// Accept list is rejected with 400 by the deployed service.
		if r.Header.Get("Accept") != "image/png" {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != "POST" || r.Header.Get("Content-Type") != "image/tiff" || !bytes.Equal(body, input) {
			t.Error("incorrect upload")
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "source.tiff")
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := prepareImage(context.Background(), path, 1, 1, options{houdiniURL: server.URL, timeout: time.Second})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%v", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := optimize(context.Background(), bytes.NewReader(input), "image/tiff", options{houdiniURL: redirect.URL, timeout: time.Second}); err == nil {
		t.Fatal("accepted redirect")
	}
	if _, err := validateImage([]byte("not an image")); err == nil {
		t.Fatal("accepted invalid image")
	}
}

func TestImagePreparationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "potentially sensitive document text", http.StatusBadRequest)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "page.png")
	if err := os.WriteFile(path, testPNG(t), 0600); err != nil {
		t.Fatal(err)
	}
	r := scanPage(context.Background(), path, 1, 1, options{houdiniURL: server.URL, timeout: time.Second})
	if r.Status != "error" || r.Error != "image preparation failed: houdini returned HTTP 400; page requires review" {
		t.Fatalf("expected actionable preparation error without response body, got %+v", r)
	}
}

// This integration check uses real Houdini conversion and a local mock Ollama.
// No document or request is sent to an external model.
func TestDockerPipeline(t *testing.T) {
	if os.Getenv("HAWKEYE_DOCKER_TEST") != "1" {
		t.Skip("set HAWKEYE_DOCKER_TEST=1 to test real Docker conversion")
	}
	dir := t.TempDir()
	pdf := filepath.Join(dir, "two pages.pdf")
	if err := os.WriteFile(pdf, syntheticPDF(), 0600); err != nil {
		t.Fatal(err)
	}
	o := options{depth: -1, dpi: 100, maxEdge: 1200, timeout: time.Minute, image: envDefault("HAWKEYE_TEST_IMAGE", defaultImage), model: "glm-ocr:bf16", analysisModel: "qwen3.5:latest", output: filepath.Join(dir, "report.jsonl")}
	count, err := pageCount(context.Background(), pdf, o)
	if err != nil || count != 2 {
		t.Fatalf("page count %d: %v", count, err)
	}
	first, err := prepareImage(context.Background(), pdf, 1, count, o)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareImage(context.Background(), pdf, 2, count, o)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("rendered first page twice")
	}
	// Confirm all TIFF frames are enumerated and independently rendered as well.
	cmd := exec.Command("docker", "run", "--rm", "--network=none", "--entrypoint=magick", o.image, "-size", "32x32", "xc:white", "xc:black", "tiff:-")
	tiff, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	tifPath := filepath.Join(dir, "multi.tiff")
	if err := os.WriteFile(tifPath, tiff, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := pageCount(context.Background(), tifPath, o); err != nil || n != 2 {
		t.Fatalf("TIFF count %d: %v", n, err)
	}
	a, err := prepareImage(context.Background(), tifPath, 1, 2, o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := prepareImage(context.Background(), tifPath, 2, 2, o)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("rendered first TIFF frame twice")
	}
	calls := 0
	var retrySuccess atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			_, _ = io.WriteString(w, `{"capabilities":["completion","vision"]}`)
			return
		}
		var request struct {
			Model, Prompt string
			Images        []string
			Stream        bool
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/api/generate" || (request.Model != o.model && request.Model != o.analysisModel) || len(request.Images) != 1 || request.Stream {
			t.Error("incorrect HTR request")
		}
		calls++
		if retrySuccess.Load() {
			response := negativeAssessment
			if request.Model == o.model {
				response = "A garden."
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": response})
			return
		}
		switch calls {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"response": "Account number: 12345678", "done": true})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{"response": negativeAssessment})
		default:
			http.Error(w, "secret document text", 500)
		}
	}))
	defer server.Close()
	o.endpoint = server.URL
	t.Setenv("OLLAMA_URL", server.URL)
	t.Setenv("LOG_LEVEL", "DEBUG")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), pdf, o, &stdout, &stderr); err == nil {
		t.Fatal("failed page must cause nonzero exit")
	}
	for _, want := range []string{"Document 1/1", "Counting pages in Docker", "Page 1/2", "Rendering page in Docker", "OCR (glm-ocr:bf16)", "Assessment (qwen3.5:latest)", "Page 1/2: review", "Page 2/2: error", "level=DEBUG", "Scan configuration", "image_bytes=", "Ollama HTTP 500"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing progress/debug message %q in %s", want, stderr.String())
		}
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "12345678") || strings.Contains(stderr.String(), "secret document text") {
		t.Fatal("progress used stdout or leaked sensitive content")
	}
	report, err := os.ReadFile(o.output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), "12345678") || strings.Contains(string(report), "secret document") {
		t.Fatal("sensitive text leaked")
	}
	var records []result
	dec := json.NewDecoder(bytes.NewReader(report))
	for dec.More() {
		var r result
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if len(records) != 2 || records[0].Page != 1 || records[0].Status != "review" || records[1].Page != 2 || records[1].Status != "error" {
		t.Fatalf("bad records: %+v", records)
	}
	info, err := os.Stat(o.output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("report permissions")
	}
	if err := run(context.Background(), pdf, o, &stdout, &stderr); err == nil {
		t.Fatal("retry failure must still be reported")
	}
	if calls != 4 || !strings.Contains(stderr.String(), "skipping completed page") {
		t.Fatal("resume repeated a successful page")
	}
	retrySuccess.Store(true)
	if err := run(context.Background(), pdf, o, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if calls != 6 {
		t.Fatalf("expected only failed page to receive OCR and assessment: %d calls", calls)
	}
	resumed, err := os.ReadFile(o.output)
	if err != nil || !bytes.HasPrefix(resumed, report) {
		t.Fatal("resume overwrote historical records")
	}
	if err := run(context.Background(), pdf, o, &stdout, &stderr); err != nil || calls != 6 {
		t.Fatalf("completed file was not skipped: %v", err)
	}
	t.Run("request limit and resume", func(t *testing.T) {
		images := t.TempDir()
		for _, name := range []string{"a.png", "b.png"} {
			if err := os.WriteFile(filepath.Join(images, name), first, 0600); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{pdf, images} {
			for _, limit := range []int{2, 3} {
				limited := o
				limited.limit = limit
				limited.output = filepath.Join(t.TempDir(), "report.jsonl")
				startCalls := calls
				for iteration, wantCalls := range []int{2, 4, 4} {
					stderr.Reset()
					if err := run(context.Background(), path, limited, &stdout, &stderr); err != nil {
						t.Fatal(err)
					}
					if calls-startCalls != wantCalls {
						t.Fatalf("limit %d run %d: got %d calls, want %d", limit, iteration, calls-startCalls, wantCalls)
					}
					if strings.Contains(stderr.String(), "Request limit reached") != (iteration == 0) {
						t.Fatalf("incorrect stop message: %s", stderr.String())
					}
					data, err := os.ReadFile(limited.output)
					if err != nil {
						t.Fatal(err)
					}
					var records []result
					if _, err := readReport(bytes.NewReader(data), func(r result) { records = append(records, r) }); err != nil {
						t.Fatal(err)
					}
					if len(records) != wantCalls/2 {
						t.Fatalf("lost or repeated page records: %+v", records)
					}
					for _, r := range records {
						if r.Status != "no_findings" || r.Assessment == nil {
							t.Fatalf("limit interrupted a page: %+v", r)
						}
					}
				}
			}
		}
		limited := o
		limited.limit = 2
		limited.output = filepath.Join(t.TempDir(), "report.jsonl")
		retrySuccess.Store(false)
		defer retrySuccess.Store(true)
		startCalls := calls
		if err := run(context.Background(), pdf, limited, &stdout, &stderr); err == nil || calls-startCalls != 1 {
			t.Fatalf("failed OCR must count, leaving too little allowance for another page: err=%v calls=%d", err, calls-startCalls)
		}
	})
	t.Run("interrupted report", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = io.WriteString(w, `{"capabilities":["completion","vision"]}`)
				return
			}
			calls++
			if calls == 1 || calls == 3 {
				_ = json.NewEncoder(w).Encode(map[string]any{"response": "Account number: 12345678", "done": true})
				return
			}
			if calls == 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"response": negativeAssessment})
				return
			}
			// Same context cancellation used by main's Ctrl+C handler, while assessment
			// on the second page is still in flight, after OCR has completed.
			cancel()
		}))
		defer server.Close()
		options := o
		options.endpoint = server.URL
		options.output = filepath.Join(dir, "interrupted.jsonl")
		options.includeText = true
		if err := run(ctx, pdf, options, &stdout, &stderr); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
		report, err := os.ReadFile(options.output)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(report))
		var first, interrupted result
		if err := dec.Decode(&first); err != nil {
			t.Fatal(err)
		}
		if err := dec.Decode(&interrupted); err != nil {
			t.Fatal(err)
		}
		if first.Page != 1 || first.Status != "review" || interrupted.Page != 2 || interrupted.Status != "error" || interrupted.Text != "Account number: 12345678" {
			t.Fatalf("lost completed page or interruption record: %+v %+v", first, interrupted)
		}
	})
}

type progressBuffer struct {
	bytes.Buffer
	heartbeat chan struct{}
}

func (b *progressBuffer) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	if bytes.Contains(p, []byte("still running")) {
		select {
		case b.heartbeat <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestProgressHeartbeat(t *testing.T) {
	output := &progressBuffer{heartbeat: make(chan struct{}, 1)}
	stop := reportProgress(output, "OCR", time.Millisecond)
	select {
	case <-output.heartbeat:
	case <-time.After(time.Second):
		stop()
		t.Fatal("no heartbeat while operation is pending")
	}
	stop() // Joins the writer before examining its output; race detector checks this.
	if !strings.Contains(output.String(), "OCR...\n") || !strings.Contains(output.String(), "OCR: still running") {
		t.Fatalf("missing stage or heartbeat: %s", output.String())
	}
}

func TestDebugLevelAndErrorRedaction(t *testing.T) {
	for _, level := range []string{"", "INFO", "DEBUG"} {
		var output bytes.Buffer
		logger, err := debugLogger(&output, level)
		if err != nil {
			t.Fatal(err)
		}
		logger.Debug("diagnostic")
		if (output.Len() > 0) != (level == "DEBUG") {
			t.Fatalf("unexpected debug output for level %q", level)
		}
	}
	if _, err := debugLogger(io.Discard, "invalid"); err == nil {
		t.Fatal("invalid log level accepted")
	}
	for _, tc := range []struct{ input, want string }{
		{"ollama API error: 404 - secret document text", "Ollama HTTP 404"},
		{"ollama API error: 401 - secret document text", "authentication or access denied"},
		{"failed to parse JSON response: secret document text - body: secret document text", "Ollama returned invalid JSON"},
		{"no response from Ollama - body: secret document text", "missing the response field"},
		{"unexpected secret document text", "Ollama request or response read failed"},
	} {
		got := ollamaFailure(errors.New(tc.input))
		if !strings.Contains(got, tc.want) || strings.Contains(got, "secret document text") {
			t.Fatalf("incorrect/redaction-unsafe error: %s", got)
		}
	}
	for _, kind := range []providers.ErrorKind{providers.ErrorInvalidRequest, providers.ErrorInvalidResponse, providers.ErrorTransport, providers.ErrorTimeout, providers.ErrorCanceled, providers.ErrorUpstream} {
		err := fmt.Errorf("wrapped: %w", providers.NewError(kind, 503, false, errors.New("secret document text")))
		got := ollamaFailure(err)
		if !strings.Contains(got, string(kind)) || !strings.Contains(got, "503") || strings.Contains(got, "secret document text") {
			t.Fatalf("lost typed error details or leaked content: %s", got)
		}
	}
}

func syntheticPDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents 5 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents 6 0 R >>",
	}
	for _, s := range []string{"1 0 0 rg 0 0 300 300 re f\n", "0 0 1 rg 0 0 300 300 re f\n"} {
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(s), s))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}

func TestPageAssessmentAndFailures(t *testing.T) {
	imageData := testPNG(t)
	input := filepath.Join(t.TempDir(), "page.png")
	if err := os.WriteFile(input, imageData, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text, assessment, status string
		includeText, noRegex           bool
	}{
		{"no findings", "A letter about the garden.", "", "no_findings", false, false},
		{"empty OCR", "", "", "error", true, false},
		{"include text", "A letter about the garden.", "", "no_findings", true, false},
		{"visual finding", "Unremarkable OCR.", `{"contains_sensitive":true,"readable":true,"sensitive_likelihood_percent":80,"kinds":["bank_account"]}`, "review", true, false},
		{"rules survive model negative", "Account number: 12345678", "", "review", true, false},
		{"unreadable", "Some text", `{"contains_sensitive":false,"readable":false,"sensitive_likelihood_percent":0,"kinds":[]}`, "review", false, false},
		{"invalid assessment preserves OCR", "Account number: 12345678", `{}`, "error", true, false},
		{"wrong assessment type preserves OCR", "Account number: 12345678", `"invalid"`, "error", true, false},
		{"model only", "Account number: 12345678", "", "no_findings", true, true},
		{"assessment HTTP failure preserves OCR", "Account number: 12345678", "HTTP500", "error", true, false},
		{"assessment cancellation preserves OCR", "Account number: 12345678", "cancel", "error", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response := tc.assessment
			if response == "" {
				response = negativeAssessment
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/image" {
					_, _ = w.Write(imageData)
					return
				}
				call := calls.Add(1)
				var request struct {
					Model, Prompt string
					Images        []string
					Format        json.RawMessage
					Think         *bool
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/api/generate" || len(request.Images) != 1 {
					t.Error("missing page image")
				}
				if call == 1 {
					if request.Model != "test-ocr" || request.Prompt != ocrPrompt || len(request.Format) != 0 || request.Think != nil {
						t.Error("OCR must use its plain transcription prompt without a JSON schema")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"response": tc.text})
					return
				}
				quoted, _ := json.Marshal(tc.text)
				if request.Model != "test-analysis" || request.Prompt != analysisPrompt+string(quoted) || len(request.Format) == 0 || request.Think == nil || *request.Think {
					t.Error("assessment must receive the image, quoted OCR, and assessment schema")
				}
				if response == "HTTP500" {
					http.Error(w, "secret document text", http.StatusInternalServerError)
					return
				}
				if response == "cancel" {
					cancel()
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"response": response, "done": true})
			}))
			defer server.Close()
			t.Setenv("OLLAMA_URL", server.URL)
			got := scanPage(ctx, input, 1, 1, options{model: "test-ocr", analysisModel: "test-analysis", houdiniURL: server.URL + "/image", timeout: time.Second, includeText: tc.includeText, noRegex: tc.noRegex})
			wantCalls := int32(2)
			if tc.text == "" {
				wantCalls = 1 // HTR rejects an empty response before assessment.
				if !strings.Contains(got.Error, "invalid_response") {
					t.Fatalf("missing empty-response diagnostic: %s", got.Error)
				}
			}
			if calls.Load() != wantCalls {
				t.Fatalf("expected %d model calls, got %d", wantCalls, calls.Load())
			}
			if got.Status != tc.status {
				t.Fatalf("got %+v", got)
			}
			if tc.includeText && got.Text != tc.text {
				t.Fatal("missing OCR text")
			}
			if !tc.includeText && got.Text != "" {
				t.Fatal("unexpected OCR text")
			}
			if got.Status != "error" && got.Assessment == nil {
				t.Fatal("missing model assessment")
			}
			if strings.Contains(got.Error, "secret document text") {
				t.Fatal("assessment error leaked response content")
			}
			if strings.Contains(tc.name, "preserves OCR") && len(got.Findings) == 0 {
				t.Fatal("lost rule findings from retained transcription")
			}
		})
	}
}

func TestRequestLimitFlag(t *testing.T) {
	cmd := newCommand()
	limit, err := cmd.Flags().GetInt("limit")
	if err != nil || limit != 0 {
		t.Fatalf("default limit = %d, err = %v", limit, err)
	}
	for _, value := range []string{"-1", "1"} {
		cmd := newCommand()
		cmd.SetArgs([]string{"--limit", value})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "limit must be") {
			t.Fatalf("invalid limit %s accepted: %v", value, err)
		}
	}
}

func TestRequestDelay(t *testing.T) {
	cmd := newCommand()
	delay, err := cmd.Flags().GetDuration("request-delay")
	if err != nil || delay != 2*time.Second {
		t.Fatalf("default request delay = %s, err = %v", delay, err)
	}
	cmd.SetArgs([]string{"--request-delay=-1s"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "request-delay must be nonnegative") {
		t.Fatalf("negative request delay was not rejected: %v", err)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"response":"synthetic text"}`)
	}))
	defer server.Close()
	t.Setenv("OLLAMA_URL", server.URL)
	var progress bytes.Buffer
	o := options{requestDelay: 20 * time.Millisecond, timeout: time.Second, progress: &progress}
	encoded := base64.StdEncoding.EncodeToString(testPNG(t))
	for i, stage := range []string{"OCR", "Assessment", "OCR"} {
		started := time.Now()
		_, err := o.extract(context.Background(), encoded, stage, "test", "test", nil)
		if (err != nil) != (i == 0) {
			t.Fatalf("request %d: %v", i, err)
		}
		if time.Since(started) < o.requestDelay {
			t.Fatalf("request %d did not wait", i)
		}
	}
	if !strings.Contains(progress.String(), "Waiting 20ms before Assessment") {
		t.Fatalf("missing wait progress: %s", progress.String())
	}
	o.requestDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := o.extract(ctx, encoded, "OCR", "test", "test", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait did not return context error: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("canceled wait sent a request: %d calls", calls.Load())
	}
}

func TestContextWindow(t *testing.T) {
	imageData := testPNG(t)
	input := filepath.Join(t.TempDir(), "page.png")
	if err := os.WriteFile(input, imageData, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"default", nil, 16384},
		{"override", []string{"--num-ctx", "32768"}, 32768},
		{"server default", []string{"--num-ctx", "0"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newCommand()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			numCtx, err := cmd.Flags().GetInt("num-ctx")
			if err != nil || numCtx != tc.want {
				t.Fatalf("num-ctx = %d, err = %v", numCtx, err)
			}
			var calls atomic.Int32
			response := negativeAssessment
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/image" {
					_, _ = w.Write(imageData)
					return
				}
				call := calls.Add(1)
				var request struct {
					Options map[string]int `json:"options"`
					Format  struct {
						Required []string
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				got, present := request.Options["num_ctx"]
				if call == 1 && request.Format.Required != nil {
					t.Error("OCR must not be forced into the assessment schema")
				}
				if call == 2 && !slices.Equal(request.Format.Required, []string{"contains_sensitive", "readable", "sensitive_likelihood_percent", "kinds"}) {
					t.Error("assessment schema must require every field")
				}
				if got != tc.want || present != (tc.want > 0) {
					t.Errorf("num_ctx = %d, present = %v; want %d", got, present, tc.want)
				}
				if call == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"response": "A garden."})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"response": response})
			}))
			defer server.Close()
			t.Setenv("OLLAMA_URL", server.URL)
			got := scanPage(context.Background(), input, 1, 1, options{model: "test-ocr", analysisModel: "test-analysis", numCtx: numCtx, houdiniURL: server.URL + "/image", timeout: time.Second})
			if got.Status != "no_findings" || calls.Load() != 2 {
				t.Fatalf("result = %+v, requests = %d", got, calls.Load())
			}
		})
	}

	cmd := newCommand()
	cmd.SetArgs([]string{"--num-ctx", "-1"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "num-ctx must be nonnegative") {
		t.Fatalf("negative num-ctx was not rejected: %v", err)
	}
}

func TestVisionModelValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"vision", `{"capabilities":["completion","vision"]}`, "", 200},
		{"text only", `{"capabilities":["completion","thinking"]}`, "does not support images", 200},
		{"missing capabilities", `{}`, "omitted capabilities", 200},
		{"wrong endpoint", `<html>secret document text</html>`, "invalid model metadata", 200},
		{"missing model", `secret document text`, "HTTP 404", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || r.URL.Path != "/api/show" || request.Model != "test-model" {
					t.Error("incorrect capabilities request")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			err := validateVisionModel(context.Background(), options{endpoint: server.URL, timeout: time.Second}, "test-model")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret document text") {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestTextOnlyModelStopsBeforeScanning(t *testing.T) {
	for _, stage := range []string{"OCR", "assessment"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "page.png")
			if err := os.WriteFile(input, testPNG(t), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/show" {
					t.Error("document submitted before validation")
					http.Error(w, "unexpected request", 500)
					return
				}
				var request struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				capabilities := []string{"completion", "vision"}
				if request.Model == "text-only" {
					capabilities = []string{"completion"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": capabilities})
			}))
			defer server.Close()
			t.Setenv("OLLAMA_URL", server.URL)
			o := options{endpoint: server.URL, model: "vision", analysisModel: "vision", image: defaultImage, dpi: 200, timeout: time.Second, output: filepath.Join(dir, "report.jsonl")}
			if stage == "OCR" {
				o.model = "text-only"
			} else {
				o.analysisModel = "text-only"
			}
			err := run(context.Background(), input, o, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "does not support images") {
				t.Fatalf("unsupported model was not rejected: %v", err)
			}
			if _, err := os.Stat(o.output); !os.IsNotExist(err) {
				t.Fatalf("report created before model validation: %v", err)
			}
		})
	}
}
