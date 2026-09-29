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
}

type reportView struct {
	ID, Directory, Name, Relative, Updated, Problem string
	Files, CompleteFiles, Pages, Review, Errors     int
	Partial                                         bool
	Rows                                            []reportRow
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
	c, err := readReport(f, func(r result) {
		r.Text = ""
		if latest[r.File] == nil || r.Page == 0 {
			latest[r.File] = make(map[int]result)
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
		if c.fileDone(file) {
			v.CompleteFiles++
		}
		for page, r := range pages {
			if page > 0 && r.Status != "error" {
				v.Pages++
			}
			if r.Status == "review" {
				v.Review++
			}
			if r.Status == "error" {
				v.Errors++
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
			v.Rows = append(v.Rows, reportRow{File: r.File, Page: r.Page, Status: r.Status, Kinds: strings.Join(kinds, ", "), Details: details})
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
