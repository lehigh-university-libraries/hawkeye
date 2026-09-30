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
	depth, dpi, maxEdge, numCtx, limit                        int
	endpoint, model, analysisModel, image, houdiniURL, output string
	timeout, requestDelay                                     time.Duration
	includeText, noRegex                                      bool
	retryErrors                                               bool
	retryDelays                                               []time.Duration
	onRetry                                                   func(stage string, attempt int, delay time.Duration, err error) error
	progress                                                  io.Writer
	logger                                                    *slog.Logger
	requests                                                  *int
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
	var providerError *providers.Error
	if errors.As(err, &providerError) {
		// HTR's typed errors are already redacted and retain category/status.
		if providerError.StatusCode >= 400 {
			return fmt.Sprintf("Ollama HTTP %d (%s; check the Ollama server logs)", providerError.StatusCode, providerError.Kind)
		}
		return "Ollama " + providerError.Error()
	}
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
	File            string      `json:"file"`
	Page            int         `json:"page"`
	PageCount       int         `json:"page_count,omitempty"`
	Status          string      `json:"status"`
	OCRModel        string      `json:"ocr_model"`
	AnalysisModel   string      `json:"analysis_model,omitempty"`
	Text            string      `json:"text,omitempty"`
	Findings        []finding   `json:"findings"`
	Assessment      *assessment `json:"assessment,omitempty"`
	Error           string      `json:"error,omitempty"`
	RetryCount      int         `json:"retry_count,omitempty"`
	DeadLetter      bool        `json:"dead_letter,omitempty"`
	RequestRetries  int         `json:"request_retries,omitempty"`
	RetryStage      string      `json:"retry_stage,omitempty"`
	RetryAttempt    int         `json:"retry_attempt,omitempty"`
	RetryAt         time.Time   `json:"retry_at,omitzero"`
	DocumentStopped bool        `json:"document_stopped,omitempty"`
	CircuitOpen     bool        `json:"circuit_open,omitempty"`
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
	f.StringVar(&o.model, "model", "glm-ocr:bf16", "Vision model for plain OCR transcription")
	f.StringVar(&o.analysisModel, "analysis-model", "qwen3.5:latest", "Vision model to assess the page image and OCR transcription")
	f.IntVar(&o.numCtx, "num-ctx", 16384, "Ollama context window in tokens; 0 uses the server/model default")
	f.StringVar(&o.houdiniURL, "houdini-url", "", "Optional on-prem Houdini image optimization endpoint (raw POST)")
	f.StringVar(&o.image, "docker-image", defaultImage, "Houdini image containing ImageMagick and Ghostscript")
	f.IntVar(&o.dpi, "dpi", 200, "PDF render DPI")
	f.IntVar(&o.maxEdge, "max-edge", 2400, "Maximum image edge in pixels; 0 preserves rendered size")
	f.DurationVar(&o.timeout, "timeout", 5*time.Minute, "Timeout for each conversion or API request")
	f.DurationVar(&o.requestDelay, "request-delay", 2*time.Second, "Pause before each Ollama inference request; 0 disables the pause")
	f.IntVar(&o.limit, "limit", 0, "Maximum Ollama inference attempts per run; stop between pages (0 = unlimited, minimum 2)")
	f.StringVarP(&o.output, "output", "o", "hawkeye-report.jsonl", "JSONL report to create or resume, or - for stdout")
	f.BoolVar(&o.includeText, "include-text", true, "Retain OCR text in report; use --include-text=false to omit it")
	f.BoolVar(&o.noRegex, "no-regex", false, "Disable text rules; retain model assessment")
	f.BoolVar(&o.retryErrors, "retry-errors", false, "Retry recorded failures first (automatic when resuming an existing report)")
	cmd.AddCommand(newServeCommand())
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
	if o.depth < -1 || o.dpi < 1 || o.maxEdge < 0 || o.timeout <= 0 || strings.TrimSpace(o.model) == "" || strings.TrimSpace(o.analysisModel) == "" || o.image == "" {
		return errors.New("invalid depth, DPI, maximum edge, timeout, OCR/analysis model, or Docker image")
	}
	if o.numCtx < 0 {
		return errors.New("num-ctx must be nonnegative; 0 uses the server/model default")
	}
	if o.requestDelay < 0 {
		return errors.New("request-delay must be nonnegative; 0 disables the pause")
	}
	if o.limit < 0 || o.limit == 1 {
		return errors.New("limit must be 0 (unlimited) or at least 2 for OCR and assessment")
	}
	if o.retryErrors && o.output == "-" {
		return errors.New("retry-errors requires an existing output report, not stdout")
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
	checkpoint := &reportCheckpoint{}
	if o.output != "-" {
		checkpoint, err = loadReport(o.output)
		if err != nil {
			return fmt.Errorf("load report: %w", err)
		}
		defer func() {
			if checkpoint.file != nil {
				_ = checkpoint.file.Close()
			}
		}()
	}
	if o.retryErrors && checkpoint.file == nil {
		return errors.New("retry-errors requires an existing output report")
	}
	remaining := 0
	for _, file := range files {
		if !checkpoint.fileDone(file) {
			remaining++
		}
	}
	if err := os.Setenv("OLLAMA_URL", strings.TrimRight(o.endpoint, "/")); err != nil {
		return err
	}
	o.debug("Scan configuration", "ollama_url", o.endpoint, "model", o.model, "analysis_model", o.analysisModel, "num_ctx", o.numCtx, "houdini_url", o.houdiniURL, "docker_image", o.image, "dpi", o.dpi, "max_edge", o.maxEdge, "timeout", o.timeout, "request_delay", o.requestDelay, "limit", o.limit, "documents", len(files))
	models := []string{o.model, o.analysisModel}
	if remaining == 0 {
		models = nil
	}
	for _, model := range models {
		stop := o.startProgress(fmt.Sprintf("Checking image support (%s)", model))
		err = validateVisionModel(ctx, o, model)
		stop()
		if err != nil {
			return err
		}
		o.debug("Model supports images", "model", model)
	}
	writer := stdout
	var report *os.File
	if o.output != "-" {
		if err := checkpoint.prepareAppend(o.output); err != nil {
			return fmt.Errorf("prepare report: %w", err)
		}
		if checkpoint.partial {
			fmt.Fprintln(stderr, "Recovered report: discarded an incomplete final JSONL record")
		}
		report = checkpoint.file
		writer = report
	}
	enc := json.NewEncoder(writer)
	failed, reviewed, pages := 0, 0, 0
	failedDocuments := 0
	circuitOpen := false
	pageCounts := make(map[string]int)
	skippedFiles, skippedPages := 0, 0
	o.requests = new(int)
	limitReached := false
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
scan:
	for _, retrying := range []bool{true, false} {
		if retrying {
			if !slices.ContainsFunc(files, checkpoint.hasErrors) {
				continue
			}
			fmt.Fprintln(stderr, "Retrying recorded failures before new pages")
		}
		for i, file := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if retrying && !checkpoint.hasErrors(file) {
				continue
			}
			fmt.Fprintf(stderr, "Document %d/%d: %q\n", i+1, len(files), filepath.Base(file))
			if checkpoint.fileDone(file) {
				fmt.Fprintln(stderr, "    Skipping: already completed or dead-lettered in report")
				skippedFiles++
				continue
			}
			// Enumeration failures are handled entirely in the retry pass.
			if !retrying && checkpoint.pageFailed(file, 0) {
				continue
			}
			if o.limit > 0 && o.limit-*o.requests < 2 {
				limitReached = true
				break scan
			}
			count := pageCounts[file]
			if count < 0 {
				continue // Enumeration already failed during the retry pass.
			}
			var err error
			if count == 0 {
				count, err = pageCount(ctx, file, o)
				pageCounts[file] = count
			}
			if err != nil {
				pageCounts[file] = -1
				fmt.Fprintf(stderr, "  Error counting pages: %v\n", err)
				retries := checkpoint.pageState(file, 0).retries
				if checkpoint.pageFailed(file, 0) && ctx.Err() == nil {
					retries++
				}
				r := result{File: file, Status: "error", OCRModel: o.model, Findings: []finding{}, RetryCount: retries, Error: fmt.Sprintf("page enumeration failed: %v; document requires review", err)}
				markDeadLetter(&r)
				if err := write(r); err != nil {
					return err
				}
				continue
			}
			consecutiveFailures := 0
			attempted := false
			for page := 1; page <= count; page++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				if checkpoint.pageDone(file, page) {
					if !retrying {
						fmt.Fprintf(stderr, "  Page %d/%d: skipping completed page or dead letter\n", page, count)
						skippedPages++
					}
					continue
				}
				if checkpoint.pageFailed(file, page) != retrying {
					continue
				}
				// Reserve assessment's first request before starting OCR.
				if o.limit > 0 && o.limit-*o.requests < 2 {
					limitReached = true
					break scan
				}
				fmt.Fprintf(stderr, "  Page %d/%d\n", page, count)
				pageStarted := time.Now()
				retries := checkpoint.pageState(file, page).retries
				requestRetries := 0
				var progressErr error
				pageOptions := o
				pageOptions.onRetry = func(stage string, attempt int, delay time.Duration, err error) error {
					requestRetries++
					pending := result{File: file, Page: page, PageCount: count, Status: "retrying", OCRModel: o.model, AnalysisModel: o.analysisModel,
						Findings: []finding{}, Error: ollamaFailure(err), RetryCount: retries, RequestRetries: requestRetries, RetryStage: stage, RetryAttempt: attempt, RetryAt: time.Now().UTC().Add(delay)}
					// Progress records do not count as failed page-processing passes.
					progressErr = saveResult(enc, report, pending)
					return progressErr
				}
				r := scanPage(ctx, file, page, count, pageOptions)
				if progressErr != nil {
					return progressErr
				}
				if retrying && r.Status == "error" && ctx.Err() == nil {
					retries++
				}
				r.RetryCount = retries
				r.RequestRetries = requestRetries
				if !retrying && ctx.Err() == nil && r.Status == "error" && consecutiveFailures == 2 {
					r.DocumentStopped = true
					r.CircuitOpen = failedDocuments == 2
				}
				markDeadLetter(&r)
				if err := write(r); err != nil {
					return err
				}
				fmt.Fprintf(stderr, "  Page %d/%d: %s (%d findings, %s)\n", page, count, r.Status, len(r.Findings), time.Since(pageStarted).Round(time.Second))
				if r.Error != "" {
					fmt.Fprintf(stderr, "    %s\n", r.Error)
				}
				// Old failures never affect the circuit breaker.
				if retrying {
					continue
				}
				attempted = true
				if r.Status == "error" {
					consecutiveFailures++
				} else {
					consecutiveFailures = 0
				}
				if consecutiveFailures == 3 {
					fmt.Fprintln(stderr, "    Three consecutive pages failed: skipping the rest of this document")
					break
				}
			}
			if !retrying && attempted {
				if consecutiveFailures == 3 {
					failedDocuments++
				} else {
					failedDocuments = 0
				}
				if failedDocuments == 3 {
					circuitOpen = true
					break scan
				}
			}
		}
	}
	fmt.Fprintf(stderr, "%d pages; %d flagged for review; %d processing errors; %s elapsed\n", pages, reviewed, failed, time.Since(started).Round(time.Second))
	if skippedFiles > 0 || skippedPages > 0 {
		fmt.Fprintf(stderr, "Resumed report: skipped %d completed files and %d completed pages in remaining files\n", skippedFiles, skippedPages)
	}
	if o.limit > 0 {
		fmt.Fprintf(stderr, "Ollama inference attempts: %d/%d\n", *o.requests, o.limit)
	}
	if limitReached {
		fmt.Fprintln(stderr, "Request limit reached: stopping before the next page; rerun with the same report to continue")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if circuitOpen {
		return errors.New("circuit breaker: 3 consecutive documents failed; see error records in the report")
	}
	if failed > 0 {
		return errors.New("scan incomplete: see error records in the report")
	}
	return nil
}

func markDeadLetter(r *result) {
	if r.Status == "error" && r.RetryCount > 10 {
		r.DeadLetter = true
		r.Error += "; dead-lettered after more than 10 retries; automatic retries disabled"
	}
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
	r := result{File: file, Page: page, PageCount: count, Status: "error", OCRModel: o.model, AnalysisModel: o.analysisModel, Findings: []finding{}}
	data, err := prepareImage(ctx, file, page, count, o)
	if err != nil {
		// Preparation errors omit HTTP bodies and conversion stderr; keep their
		// operational details so a bad endpoint or failed conversion is actionable.
		r.Error = fmt.Sprintf("image preparation failed: %v; page requires review", err)
		o.debug("Image preparation failed", "page", page, "error", r.Error)
		return r
	}
	o.debug("Image prepared", "page", page, "image_bytes", len(data))
	encoded := base64.StdEncoding.EncodeToString(data)
	text, err := o.extract(ctx, encoded, "OCR", o.model, ocrPrompt, nil)
	if err != nil {
		r.Error = fmt.Sprintf("OCR request failed: %s; page requires review", ollamaFailure(err))
		return r
	}
	// Save OCR and run independent rules before analysis, including on analysis
	// failure or cancellation. The analysis model never rewrites the transcript.
	if o.includeText {
		r.Text = text
	}
	if !o.noRegex {
		r.Findings = detect(text)
	}
	if strings.TrimSpace(text) == "" {
		r.Findings = append(r.Findings, finding{Kind: "empty_ocr", Source: "ocr"})
	}
	quoted, _ := json.Marshal(text) // A string is always JSON-encodable.
	raw, err := o.extract(ctx, encoded, "Assessment", o.analysisModel, analysisPrompt+string(quoted), json.RawMessage(assessmentSchema))
	if err != nil {
		r.Error = fmt.Sprintf("assessment request failed: %s; page requires review", ollamaFailure(err))
		return r
	}
	r.Assessment, err = parseAssessment(raw)
	if err != nil {
		r.Error = fmt.Sprintf("invalid assessment response: %v; page requires review", err)
		o.debug("Model response validation failed", "page", page, "model", o.analysisModel, "error", err.Error())
		return r
	}
	for _, kind := range r.Assessment.Kinds {
		r.Findings = append(r.Findings, finding{Kind: kind, Source: "model"})
	}
	r.Status = "no_findings"
	if len(r.Findings) > 0 || *r.Assessment.ContainsSensitive || !*r.Assessment.Readable {
		r.Status = "review"
	}
	return r
}

func (o options) extract(ctx context.Context, encoded, stage, model, prompt string, format json.RawMessage) (string, error) {
	delays := o.retryDelays
	if delays == nil {
		delays = []time.Duration{5 * time.Second, 5 * time.Second, 30 * time.Second, time.Minute, time.Minute}
	}
	requestOptions := o
	for attempt := 0; ; attempt++ {
		text, err := requestOptions.extractOnce(ctx, encoded, stage, model, prompt, format)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var providerError *providers.Error
		if err == nil || attempt == len(delays) || !errors.As(err, &providerError) ||
			(!providerError.Retryable && providerError.Kind != providers.ErrorInvalidResponse) {
			return text, err
		}
		// Keep one request available for assessment if OCR recovers.
		reserve := 0
		if stage == "OCR" {
			reserve = 1
		}
		if o.limit > 0 && o.requests != nil && o.limit-*o.requests <= reserve {
			return text, err
		}
		// Backoff supplies the inter-request pause on retries.
		requestOptions.requestDelay = 0
		delay := max(delays[attempt], o.requestDelay)
		if o.progress != nil {
			fmt.Fprintf(o.progress, "    %s failed: %s; retry %d/%d in %s\n", stage, ollamaFailure(err), attempt+1, len(delays), delay)
		}
		if o.onRetry != nil {
			if err := o.onRetry(stage, attempt+1, delay, err); err != nil {
				return "", err
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func (o options) extractOnce(ctx context.Context, encoded, stage, model, prompt string, format json.RawMessage) (string, error) {
	// Requests are sequential. Waiting here also spaces requests after failures,
	// without delaying the durable save of a completed page.
	if o.requestDelay > 0 {
		stop := o.startProgress(fmt.Sprintf("Waiting %s before %s", o.requestDelay, stage))
		timer := time.NewTimer(o.requestDelay)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		stop()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stop := o.startProgress(fmt.Sprintf("%s (%s)", stage, model))
	defer stop()
	requested := time.Now()
	config := providers.Config{Model: model, Prompt: prompt, Temperature: 0, Timeout: o.timeout, NumCtx: o.numCtx, Format: format}
	if len(format) > 0 {
		// Qwen's thinking mode can consume the structured output without a final
		// response. Request only the assessment; never parse a reasoning trace.
		config.Think = new(bool)
	}
	// Omit the source filename: HTR must detect the prepared image's MIME type
	// from its bytes, not reject a rendered page because its source was a PDF.
	if o.requests != nil {
		*o.requests++
	}
	text, usage, err := ollama.New().ExtractText(ctx, config, "", encoded)
	// HTR errors may contain document data; log only the sanitized description.
	if err != nil {
		o.debug("Model request failed", "stage", stage, "model", model, "elapsed", time.Since(requested), "error", ollamaFailure(err))
	} else {
		o.debug("Model request completed", "stage", stage, "model", model, "elapsed", time.Since(requested), "response_bytes", len(text), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
	}
	return text, err
}
