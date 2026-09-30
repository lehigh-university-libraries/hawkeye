package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

//go:embed web/*
var viewerFiles embed.FS

type reportRow struct {
	File, Status, Kinds, Details string
	Page                         int
	State, Tone, NextAction      string
	RequestRetries, RetryCount   int
}

type reportView struct {
	ID, Directory, Name, Relative, Updated, Problem                        string
	Files, CompleteFiles, Pages, Review, Errors                            int
	Partial                                                                bool
	Rows                                                                   []reportRow
	Retrying, RetriedPages, StoppedDocuments, PersistentFiles, DeadLetters int
	CircuitOpen                                                            bool
}

type viewerPage struct {
	Reports []reportView
	Report  reportView
	All     bool
}

func newServeCommand() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use: "serve [reports-directory]", Short: "Serve a read-only report dashboard",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) > 0 {
				root = args[0]
			}
			handler, err := reportHandler(root)
			if err != nil {
				return err
			}
			listener, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
			fmt.Fprintf(cmd.ErrOrStderr(), "Read-only report viewer listening on %s\n", listener.Addr())
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			select {
			case err := <-done:
				return err
			case <-cmd.Context().Done():
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := server.Shutdown(ctx); err != nil {
					_ = server.Close()
					return err
				}
				err := <-done
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			}
		},
	}
	cmd.Flags().StringVar(&addr, "listen", "127.0.0.1:8080", "HTTP listen address; use :8080 behind your authenticated reverse proxy")
	return cmd
}

func findReports(root string) (map[string]string, error) {
	paths := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil // Includes symlinks; WalkDir does not traverse them.
		}
		name := strings.ToLower(entry.Name())
		if strings.HasSuffix(name, "jsonl") || (strings.HasSuffix(name, ".json") && strings.Contains(name, "report")) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths[fmt.Sprintf("%x", sha256.Sum256([]byte(rel)))] = path
		}
		return nil
	})
	return paths, err
}

func viewReport(root, id, path string, all bool) reportView {
	rel, _ := filepath.Rel(root, path)
	dir := filepath.Dir(rel)
	if dir == "." {
		dir = filepath.Base(root)
	}
	v := reportView{ID: id, Name: filepath.Base(path), Relative: rel, Directory: dir}
	f, err := os.Open(path) // #nosec G304 -- Path comes only from server-side discovery under the configured root.
	if err != nil {
		v.Problem = "Report could not be opened."
		return v
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil {
		v.Updated = info.ModTime().UTC().Format("2006-01-02 15:04:05 UTC")
	}
	// Keep only the latest attempt for each page; never send OCR text to the UI.
	latest := make(map[string]map[int]result)
	stopped := make(map[string]bool)
	c, err := readReport(f, func(r result) {
		r.Text = ""
		v.CircuitOpen = r.CircuitOpen
		stopped[r.File] = r.DocumentStopped
		if latest[r.File] == nil || r.Page == 0 {
			previous := latest[r.File]
			latest[r.File] = make(map[int]result)
			for page, old := range previous {
				if old.DeadLetter || (old.Status == "error" && old.RetryCount > 10) {
					latest[r.File][page] = old
				}
			}
		}
		if r.Page > 0 {
			delete(latest[r.File], 0)
		}
		latest[r.File][r.Page] = r
	})
	if err != nil {
		v.Problem = err.Error()
		return v
	}
	v.Partial = c.partial
	v.Files = len(c.documents)
	for file, pages := range latest {
		complete, persistent := c.fileDone(file), false
		if stopped[file] {
			v.StoppedDocuments++
		}
		for page, r := range pages {
			r.DocumentStopped = r.DocumentStopped && stopped[file]
			r.CircuitOpen = r.CircuitOpen && v.CircuitOpen
			r.DeadLetter = r.DeadLetter || (r.Status == "error" && r.RetryCount > 10)
			if page > 0 && (r.Status == "review" || r.Status == "no_findings") {
				v.Pages++
			}
			if r.Status == "review" {
				v.Review++
			}
			if r.Status == "error" {
				v.Errors++
				complete = false
				persistent = persistent || r.RetryCount > 0 || r.DeadLetter
			}
			if r.Status == "retrying" {
				v.Retrying++
				complete = false
				persistent = persistent || r.RetryCount > 0
			}
			if r.RequestRetries > 0 {
				v.RetriedPages++
			}
			if r.DeadLetter {
				v.DeadLetters++
			}
			if !all && r.Status == "no_findings" {
				continue
			}
			var kinds []string
			for _, finding := range r.Findings {
				kinds = append(kinds, finding.Kind)
			}
			sort.Strings(kinds)
			kinds = slices.Compact(kinds)
			details := r.Error
			if r.Assessment != nil && r.Assessment.Readable != nil && !*r.Assessment.Readable {
				if details != "" {
					details += "; "
				}
				details += "Model reported unreadable text"
			}
			state, tone, action := resultState(r)
			v.Rows = append(v.Rows, reportRow{File: r.File, Page: r.Page, Status: r.Status, Kinds: strings.Join(kinds, ", "), Details: details,
				State: state, Tone: tone, NextAction: action, RequestRetries: r.RequestRetries, RetryCount: r.RetryCount})
		}
		if complete {
			v.CompleteFiles++
		}
		if persistent {
			v.PersistentFiles++
		}
	}
	sort.Slice(v.Rows, func(i, j int) bool {
		if v.Rows[i].File == v.Rows[j].File {
			return v.Rows[i].Page < v.Rows[j].Page
		}
		return v.Rows[i].File < v.Rows[j].File
	})
	return v
}

func resultState(r result) (state, tone, action string) {
	switch {
	case r.DeadLetter:
		return "Dead letter — investigate", "danger", "More than 10 failed retry passes. Automatic retries are disabled for this page; inspect the document and server logs."
	case r.Status == "retrying":
		return "Retrying request", "info", fmt.Sprintf("%s retry %d/5 scheduled for %s. This is the last saved state; an interrupted scan must be resumed.", r.RetryStage, r.RetryAttempt, r.RetryAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	case r.CircuitOpen:
		return "Possible upstream issue", "danger", "Three consecutive documents hit the failure threshold. The scan stopped; check Ollama connectivity and server logs before resuming."
	case r.DocumentStopped:
		return "Document stopped", "warning", "Three consecutive pages failed. Any remaining pages were deferred to the next run; inspect this document if failures recur."
	case r.Status == "error" && r.RetryCount > 0:
		return "Repeated failure — investigate", "warning", "This page has failed on multiple runs. Inspect the document and server logs; it remains eligible for automatic retry."
	case r.Status == "error" && r.RequestRetries > 0:
		return "Processing failed after retries", "warning", "This pass included request retries but did not complete. It will be retried first on the next run."
	case r.Status == "error":
		return "Processing error", "warning", "Review the error. This page will be retried first on the next run."
	case r.Status == "review":
		return "Review", "warning", "Review the findings. Processing completed."
	case r.RequestRetries > 0:
		return "Recovered after retries", "success", "The request recovered and processing completed."
	default:
		return "No findings", "success", ""
	}
}

// Prevent report-controlled cell values from becoming spreadsheet formulas on paste.
func sheetCell(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}

func reportHandler(root string) (http.Handler, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("reports root must be an existing directory, not a symlink")
	}
	templates, err := template.New("viewer").Funcs(template.FuncMap{"cell": sheetCell}).ParseFS(viewerFiles, "web/*.html")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	for _, asset := range []string{"app.js", "style.css"} {
		mux.HandleFunc("GET /assets/"+asset, func(w http.ResponseWriter, r *http.Request) {
			data, _ := viewerFiles.ReadFile("web/" + asset)
			if asset == "app.js" {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			} else {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			}
			_, _ = w.Write(data)
		})
	}
	render := func(w http.ResponseWriter, name string, data viewerPage) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.ExecuteTemplate(w, name, data)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		paths, err := findReports(root)
		if err != nil {
			http.Error(w, "Could not list reports", http.StatusInternalServerError)
			return
		}
		var data viewerPage
		for id, path := range paths {
			v := viewReport(root, id, path, false)
			v.Rows = nil
			data.Reports = append(data.Reports, v)
		}
		sort.Slice(data.Reports, func(i, j int) bool { return data.Reports[i].Relative < data.Reports[j].Relative })
		render(w, "index.html", data)
	})
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		paths, err := findReports(root)
		if err != nil {
			http.Error(w, "Could not list reports", http.StatusInternalServerError)
			return
		}
		id := r.URL.Query().Get("id")
		path, ok := paths[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		all := r.URL.Query().Get("all") == "1"
		render(w, "report.html", viewerPage{Report: viewReport(root, id, path, all), All: all})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	}), nil
}
