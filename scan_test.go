package main

import (
	"bytes"
	"context"
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
	"strings"
	"testing"
	"time"
)

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

func TestAssessment(t *testing.T) {
	valid := `{"contains_sensitive":true,"readable":true,"sensitive_likelihood_percent":85,"kinds":["bank_account"]}`
	if _, err := parseAssessment(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `null`, "not JSON", strings.Replace(valid, "85", "101", 1), strings.Replace(valid, "true", "false", 1), strings.Replace(valid, "bank_account", "123456789", 1)} {
		if _, err := parseAssessment(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
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
	o := options{depth: -1, dpi: 100, maxEdge: 1200, timeout: time.Minute, image: envDefault("HAWKEYE_TEST_IMAGE", defaultImage), model: "glm-ocr:bf16", output: filepath.Join(dir, "report.jsonl")}
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
		if r.URL.Path != "/api/generate" || request.Model != o.model || request.Prompt != "Text Recognition:" || len(request.Images) != 1 || request.Stream {
			t.Error("incorrect HTR request")
		}
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"response":"Account number: 12345678","done":true}`)
		} else {
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
	for _, want := range []string{"Document 1/1", "Counting pages in Docker", "Page 1/2", "Rendering page in Docker", "OCR (glm-ocr:bf16)", "Page 1/2: review", "Page 2/2: error", "level=DEBUG", "Scan configuration", "image_bytes=", "Ollama HTTP 500"} {
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
		t.Fatal("overwrote report")
	}
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
			if calls == 1 {
				_, _ = io.WriteString(w, `{"response":"Account number: 12345678","done":true}`)
				return
			}
			// Same context cancellation used by main's Ctrl+C handler, while OCR
			// on the second page is still in flight.
			cancel()
		}))
		defer server.Close()
		options := o
		options.endpoint = server.URL
		options.output = filepath.Join(dir, "interrupted.jsonl")
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
		if first.Page != 1 || first.Status != "review" || interrupted.Page != 2 || interrupted.Status != "error" {
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
		name, ocr, assessment, status string
		includeText                   bool
	}{
		{"no findings", "A letter about the garden.", "", "no_findings", false},
		{"empty OCR", "", "", "review", false},
		{"include text", "A letter about the garden.", "", "no_findings", true},
		{"visual finding", "Unremarkable OCR.", `{"contains_sensitive":true,"readable":true,"sensitive_likelihood_percent":80,"kinds":["bank_account"]}`, "review", false},
		{"rules survive model negative", "Account number: 12345678", `{"contains_sensitive":false,"readable":true,"sensitive_likelihood_percent":5,"kinds":[]}`, "review", false},
		{"unreadable", "Some text", `{"contains_sensitive":false,"readable":false,"sensitive_likelihood_percent":0,"kinds":[]}`, "review", false},
		{"invalid assessment", "Some text", `{}`, "error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/image" {
					_, _ = w.Write(imageData)
					return
				}
				calls++
				raw := tc.ocr
				if calls == 2 {
					raw = tc.assessment
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"response": raw, "done": true})
			}))
			defer server.Close()
			t.Setenv("OLLAMA_URL", server.URL)
			o := options{model: "glm-ocr:bf16", houdiniURL: server.URL + "/image", timeout: time.Second, includeText: tc.includeText}
			if tc.assessment != "" {
				o.analysisModel = "test-vision"
			}
			got := scanPage(context.Background(), input, 1, 1, o)
			if got.Status != tc.status {
				t.Fatalf("got %+v", got)
			}
			if tc.includeText && got.Text != tc.ocr {
				t.Fatal("missing OCR text")
			}
			if !tc.includeText && got.Text != "" {
				t.Fatal("unexpected OCR text")
			}
		})
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
	for _, analysis := range []bool{false, true} {
		t.Run(fmt.Sprintf("analysis=%v", analysis), func(t *testing.T) {
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
				var request struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				capabilities := []string{"completion"}
				if request.Model == "vision-model" {
					capabilities = append(capabilities, "vision")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": capabilities})
			}))
			defer server.Close()
			t.Setenv("OLLAMA_URL", server.URL)
			o := options{endpoint: server.URL, model: "gpt-oss:latest", image: defaultImage, dpi: 200, timeout: time.Second, output: filepath.Join(dir, "report.jsonl")}
			if analysis {
				o.model = "vision-model"
				o.analysisModel = "gpt-oss:latest"
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
