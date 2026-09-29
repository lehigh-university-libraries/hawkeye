package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reportBytes(t *testing.T, records ...result) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, r := range records {
		if err := json.NewEncoder(&b).Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func TestResumeCheckpoints(t *testing.T) {
	dir := t.TempDir()
	image, pdf := filepath.Join(dir, "image.png"), filepath.Join(dir, "pages.pdf")
	data := reportBytes(t,
		result{File: image, Page: 1, Status: "error"},
		result{File: image, Page: 1, Status: "review", Text: strings.Repeat("text", 20000)},
		result{File: pdf, Page: 1, PageCount: 3, Status: "no_findings"},
		result{File: pdf, Page: 2, PageCount: 3, Status: "review"},
		result{File: pdf, Page: 2, PageCount: 3, Status: "error"},
	)
	c, err := readReport(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.fileDone(image) || c.fileDone(pdf) || !c.pageDone(pdf, 1) || c.pageDone(pdf, 2) || c.pageDone(pdf, 3) {
		t.Fatal("incorrect completion state for old images or partially completed PDFs")
	}
}

func TestReportTailRecovery(t *testing.T) {
	for _, tail := range []string{"partial", "no newline", "invalid complete line", "invalid final syntax"} {
		t.Run(tail, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.jsonl")
			r := result{File: filepath.Join(dir, "image.png"), Page: 1, PageCount: 1, Status: "no_findings"}
			good := reportBytes(t, r)
			data := append([]byte(nil), good...)
			switch tail {
			case "partial":
				data = append(data, `{"file":"unfinished`...)
			case "no newline":
				data = bytes.TrimSuffix(data, []byte("\n"))
			case "invalid complete line":
				data = append(data, "{broken}\n"...)
			case "invalid final syntax":
				data = append(data, `{"file":]`...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadReport(path)
			if strings.HasPrefix(tail, "invalid") {
				if err == nil {
					_ = c.file.Close()
					t.Fatal("accepted corrupt record")
				}
				got, _ := os.ReadFile(path)
				if !bytes.Equal(got, data) {
					t.Fatal("modified corrupt report")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.file.Close()
			if !c.fileDone(r.File) || c.partial != (tail == "partial") {
				t.Fatal("lost completed record")
			}
			if err := c.prepareAppend(path); err != nil {
				t.Fatal(err)
			}
			r.File = filepath.Join(dir, "next.png")
			if err := saveResult(json.NewEncoder(c.file), c.file, r); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, append(good, reportBytes(t, r)...)) {
				t.Fatal("repair did not preserve records and append a separate line")
			}
		})
	}
}

func TestResumeCompletedDirectoryNeedsNoServices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.jsonl")
	var records []result
	for _, name := range []string{"a.png", "b.png"} {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, testPNG(t), 0600); err != nil {
			t.Fatal(err)
		}
		records = append(records, result{File: file, Page: 1, Status: "review"})
	}
	data := reportBytes(t, records...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:1")
	o := options{depth: -1, model: "ocr", analysisModel: "assessment", dpi: 200, image: "unavailable", timeout: time.Second, endpoint: "http://127.0.0.1:1", output: path}
	var progress bytes.Buffer
	if err := run(context.Background(), dir, o, io.Discard, &progress); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) || !strings.Contains(progress.String(), "skipped 2 completed files") {
		t.Fatal("completed directory was not skipped without modifying report")
	}
}
