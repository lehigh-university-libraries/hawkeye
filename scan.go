package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lehigh-university-libraries/htr/pkg/ollama"
	"github.com/lehigh-university-libraries/htr/pkg/providers"
	"github.com/spf13/cobra"
)

const defaultImage = "islandora/houdini@sha256:22f87ca3232b7edccfb0b0c4d769c77f9a97e29c1968a75a4481ed3b6369682a"

type options struct {
	depth, dpi, maxEdge                                       int
	endpoint, model, analysisModel, image, houdiniURL, output string
	timeout                                                   time.Duration
	includeText, noRegex                                      bool
	progress                                                  io.Writer
	logger                                                    *slog.Logger
}

func (o options) debug(message string, args ...any) {
	if o.logger != nil {
		o.logger.Debug(message, args...)
	}
}

func debugLogger(w io.Writer, rawLevel string) (*slog.Logger, error) {
	level := slog.LevelInfo
	if rawLevel != "" {
		if err := level.UnmarshalText([]byte(rawLevel)); err != nil {
			return nil, errors.New("invalid LOG_LEVEL: use DEBUG, INFO, WARN, or ERROR")
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), nil
}

// HTR includes server response bodies in some errors. Translate those cases
// instead of logging raw errors, even at DEBUG level.
func ollamaFailure(err error) string {
	message := err.Error()
	if strings.HasPrefix(message, "ollama API error: ") {
		status, _, _ := strings.Cut(strings.TrimPrefix(message, "ollama API error: "), " - ")
		if code, parseErr := strconv.Atoi(status); parseErr == nil && code >= 100 && code <= 599 {
			hint := "check the Ollama server logs"
			switch code {
			case 401, 403:
				hint = "authentication or access denied"
			case 404:
				hint = "check the API endpoint and installed model name"
			case 413:
				hint = "image request is too large"
			case 429:
				hint = "request rate limited"
			}
			return fmt.Sprintf("Ollama HTTP %d (%s)", code, hint)
		}
	}
	if strings.HasPrefix(message, "failed to parse JSON response:") {
		return "Ollama returned invalid JSON; check that OLLAMA_URL points to the API, not a web UI"
	}
	if strings.HasPrefix(message, "no response from Ollama") {
		return "Ollama response is missing the response field; check the API endpoint"
	}
	var transportError *url.Error
	if errors.As(err, &transportError) {
		return "Ollama connection failed: " + transportError.Error()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Ollama request interrupted: " + err.Error()
	}
	return "Ollama request or response read failed"
}

func (o options) startProgress(label string) func() {
	return reportProgress(o.progress, label, 10*time.Second)
}

// The returned stop function waits for the reporter, so successive stages never
// write concurrently and no heartbeat survives the operation it describes.
func reportProgress(w io.Writer, label string, interval time.Duration) func() {
	if w == nil {
		return func() {}
	}
	started := time.Now()
	fmt.Fprintf(w, "    %s...\n", label)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fmt.Fprintf(w, "    %s: still running (%s elapsed)\n", label, time.Since(started).Round(time.Second))
			}
		}
	}()
	return func() { close(stop); <-done }
}

type result struct {
	File          string      `json:"file"`
	Page          int         `json:"page"`
	Status        string      `json:"status"`
	OCRModel      string      `json:"ocr_model"`
	AnalysisModel string      `json:"analysis_model,omitempty"`
	Text          string      `json:"text,omitempty"`
	Findings      []finding   `json:"findings"`
	Assessment    *assessment `json:"assessment,omitempty"`
	Error         string      `json:"error,omitempty"`
}

func newCommand() *cobra.Command {
	o := options{}
	cmd := &cobra.Command{
		Version: version,
		Use:     "hawkeye [path]", Short: "Flag PDF and image pages for sensitive-information review",
		Args: cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) != 0 {
				path = args[0]
			}
			return run(cmd.Context(), path, o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	f := cmd.Flags()
	f.IntVarP(&o.depth, "depth", "d", -1, "Subdirectory levels to scan; 0 = this directory, -1 = unlimited")
	f.StringVar(&o.endpoint, "ollama-url", envDefault("OLLAMA_URL", "https://ollama.cc.lehigh.edu"), "On-prem Ollama URL")
	f.StringVar(&o.model, "model", "glm-ocr:bf16", "OCR model")
	f.StringVar(&o.analysisModel, "analysis-model", "", "Optional vision model for JSON PII assessment")
	f.StringVar(&o.houdiniURL, "houdini-url", "", "Optional on-prem Houdini image optimization endpoint (raw POST)")
	f.StringVar(&o.image, "docker-image", defaultImage, "Houdini image containing ImageMagick and Ghostscript")
	f.IntVar(&o.dpi, "dpi", 200, "PDF render DPI")
	f.IntVar(&o.maxEdge, "max-edge", 2400, "Maximum image edge in pixels; 0 preserves rendered size")
	f.DurationVar(&o.timeout, "timeout", 5*time.Minute, "Timeout for each conversion or API request")
	f.StringVarP(&o.output, "output", "o", "hawkeye-report.jsonl", "New JSONL report path, or - for stdout")
	f.BoolVar(&o.includeText, "include-text", true, "Retain OCR text in report; use --include-text=false to omit it")
	f.BoolVar(&o.noRegex, "no-regex", false, "Disable text rules (requires --analysis-model)")
	return cmd
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must be an http(s) URL without credentials, query, or fragment")
	}
	return nil
}

// HTR handles inference; /api/show supplies the model capabilities it does not
// expose. Validate before sending documents: text-only responses are not OCR.
func validateVisionModel(ctx context.Context, o options, model string) error {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	data, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.endpoint, "/")+"/api/show", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: o.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot check model %q: %w", model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cannot check model %q: Ollama /api/show returned HTTP %d; check the endpoint and installed model name", model, resp.StatusCode)
	}
	var info struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return errors.New("ollama /api/show returned invalid model metadata; check that OLLAMA_URL points to the API, not a web UI")
	}
	if info.Capabilities == nil {
		return fmt.Errorf("cannot verify image support for model %q: /api/show omitted capabilities", model)
	}
	if !slices.Contains(info.Capabilities, "vision") {
		return fmt.Errorf("model %q does not support images; choose an installed OCR or vision model", model)
	}
	return nil
}

func discover(root string, depth int) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("input must not be a symlink")
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() || mimeType(root) == "" {
			return nil, errors.New("unsupported input file")
		}
		return []string{root}, nil
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && depth >= 0 && strings.Count(rel, string(filepath.Separator))+1 > depth {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() && mimeType(path) != "" {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func run(ctx context.Context, path string, o options, stdout, stderr io.Writer) error {
	started := time.Now()
	o.progress = stderr
	var err error
	o.logger, err = debugLogger(stderr, os.Getenv("LOG_LEVEL"))
	if err != nil {
		return err
	}
	if o.depth < -1 || o.dpi < 1 || o.maxEdge < 0 || o.timeout <= 0 || strings.TrimSpace(o.model) == "" || o.image == "" {
		return errors.New("invalid depth, DPI, maximum edge, timeout, model, or Docker image")
	}
	if o.noRegex && o.analysisModel == "" {
		return errors.New("--no-regex requires --analysis-model")
	}
	if err := validateURL(o.endpoint); err != nil {
		return err
	}
	if o.houdiniURL != "" {
		if err := validateURL(o.houdiniURL); err != nil {
			return err
		}
	}
	files, err := discover(path, o.depth)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no supported PDFs or images found")
	}
	if err := os.Setenv("OLLAMA_URL", strings.TrimRight(o.endpoint, "/")); err != nil {
		return err
	}
	o.debug("Scan configuration", "ollama_url", o.endpoint, "ocr_model", o.model, "analysis_model", o.analysisModel, "houdini_url", o.houdiniURL, "docker_image", o.image, "dpi", o.dpi, "max_edge", o.maxEdge, "timeout", o.timeout, "documents", len(files))
	models := []string{o.model}
	if o.analysisModel != "" && o.analysisModel != o.model {
		models = append(models, o.analysisModel)
	}
	for _, model := range models {
		stop := o.startProgress(fmt.Sprintf("Checking image support (%s)", model))
		err := validateVisionModel(ctx, o, model)
		stop()
		if err != nil {
			return err
		}
		o.debug("Model supports images", "model", model)
	}
	writer := stdout
	var report *os.File
	if o.output != "-" {
		report, err = os.OpenFile(o.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("create report (existing files are never overwritten): %w", err)
		}
		defer report.Close()
		writer = report
	}
	enc := json.NewEncoder(writer)
	failed, reviewed, pages := 0, 0, 0
	write := func(r result) error {
		if err := saveResult(enc, report, r); err != nil {
			return err
		}
		if r.Status == "error" {
			failed++
		}
		if r.Status == "review" {
			reviewed++
		}
		if r.Page > 0 {
			pages++
		}
		return nil
	}
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Document %d/%d: %q\n", i+1, len(files), filepath.Base(file))
		count, err := pageCount(ctx, file, o)
		if err != nil {
			fmt.Fprintf(stderr, "  Error counting pages: %v\n", err)
			if err := write(result{File: file, Status: "error", OCRModel: o.model, Findings: []finding{}, Error: fmt.Sprintf("page enumeration failed: %v; document requires review", err)}); err != nil {
				return err
			}
			continue
		}
		for page := 1; page <= count; page++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "  Page %d/%d\n", page, count)
			pageStarted := time.Now()
			r := scanPage(ctx, file, page, count, o)
			if err := write(r); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "  Page %d/%d: %s (%d findings, %s)\n", page, count, r.Status, len(r.Findings), time.Since(pageStarted).Round(time.Second))
			if r.Error != "" {
				fmt.Fprintf(stderr, "    %s\n", r.Error)
			}
		}
	}
	fmt.Fprintf(stderr, "%d pages; %d flagged for review; %d processing errors; %s elapsed\n", pages, reviewed, failed, time.Since(started).Round(time.Second))
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		return errors.New("scan incomplete: see error records in the report")
	}
	return nil
}

// Each saved record is a checkpoint independent of graceful shutdown. stdout
// is owned by the caller, so durability for -o - belongs to the receiving process.
func saveResult(enc *json.Encoder, report *os.File, r result) error {
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if report != nil {
		if err := report.Sync(); err != nil {
			return fmt.Errorf("sync report: %w", err)
		}
	}
	return nil
}

func scanPage(ctx context.Context, file string, page, count int, o options) result {
	r := result{File: file, Page: page, Status: "error", OCRModel: o.model, AnalysisModel: o.analysisModel, Findings: []finding{}}
	data, err := prepareImage(ctx, file, page, count, o)
	if err != nil {
		// Preparation errors omit HTTP bodies and conversion stderr; keep their
		// operational details so a bad endpoint or failed conversion is actionable.
		r.Error = fmt.Sprintf("image preparation failed: %v; page requires review", err)
		o.debug("Image preparation failed", "page", page, "error", r.Error)
		return r
	}
	o.debug("Image prepared", "page", page, "image_bytes", len(data))
	provider := ollama.New()
	config := providers.Config{Model: o.model, Prompt: "Text Recognition:", Temperature: 0, Timeout: o.timeout}
	encoded := base64.StdEncoding.EncodeToString(data)
	stop := o.startProgress(fmt.Sprintf("OCR (%s)", o.model))
	requested := time.Now()
	text, usage, err := provider.ExtractText(ctx, config, file, encoded)
	stop()
	// HTR errors can include response bodies containing PII. Never persist them.
	if err != nil {
		r.Error = fmt.Sprintf("OCR request failed: %s; page requires review", ollamaFailure(err))
		o.debug("OCR failed", "page", page, "model", config.Model, "elapsed", time.Since(requested), "error", r.Error)
		return r
	}
	o.debug("OCR completed", "page", page, "model", config.Model, "elapsed", time.Since(requested), "text_bytes", len(text), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
	if o.includeText {
		r.Text = text
	}
	if !o.noRegex {
		r.Findings = detect(text)
	}
	if strings.TrimSpace(text) == "" {
		r.Findings = append(r.Findings, finding{Kind: "empty_ocr", Source: "ocr"})
	}
	if o.analysisModel != "" {
		config.Model, config.Prompt = o.analysisModel, analysisPrompt
		stop := o.startProgress(fmt.Sprintf("Assessing sensitive content (%s)", o.analysisModel))
		requested := time.Now()
		raw, _, err := provider.ExtractText(ctx, config, file, encoded)
		stop()
		if err != nil {
			r.Error = fmt.Sprintf("assessment request failed: %s; page requires review", ollamaFailure(err))
			o.debug("Assessment failed", "page", page, "model", config.Model, "elapsed", time.Since(requested), "error", r.Error)
			return r
		}
		a, err := parseAssessment(raw)
		if err != nil {
			r.Error = "invalid assessment response; page requires review"
			o.debug("Assessment validation failed", "page", page, "error", err.Error())
			return r
		}
		r.Assessment = &a
		o.debug("Assessment completed", "page", page, "model", config.Model, "elapsed", time.Since(requested))
		for _, kind := range a.Kinds {
			r.Findings = append(r.Findings, finding{Kind: kind, Source: "model"})
		}
	}
	r.Status = "no_findings"
	if len(r.Findings) > 0 || (r.Assessment != nil && (*r.Assessment.ContainsSensitive || !*r.Assessment.Readable)) {
		r.Status = "review"
	}
	return r
}
