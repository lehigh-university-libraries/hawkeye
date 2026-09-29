package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// This opt-in check sends only the two synthetic fixtures to the chosen server.
// It exercises the real HTR requests, without Docker or the Houdini service.
func TestLiveModels(t *testing.T) {
	if os.Getenv("HAWKEYE_OLLAMA_TEST") != "1" {
		t.Skip("set HAWKEYE_OLLAMA_TEST=1 and OLLAMA_URL to test live models")
	}
	if os.Getenv("OLLAMA_URL") == "" {
		t.Fatal("OLLAMA_URL must explicitly select the server for the live check")
	}
	cmd := newCommand()
	ocrModel, err := cmd.Flags().GetString("model")
	if err != nil {
		t.Fatal(err)
	}
	analysisModel, err := cmd.Flags().GetString("analysis-model")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		file, text, status, kind string
	}{
		{"ordinary-note.png", "Please file this garden note.", "no_findings", ""},
		{"bank-account.png", "Bank account number: 12345678", "review", "bank_account"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join("testdata", tc.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			optimizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = w.Write(data)
			}))
			defer optimizer.Close()
			o := options{model: ocrModel, analysisModel: analysisModel, numCtx: 16384, timeout: 3 * time.Minute, includeText: true, houdiniURL: optimizer.URL, progress: os.Stderr}
			got := scanPage(context.Background(), path, 1, 1, o)
			if got.Status != tc.status || got.Text != tc.text || got.Assessment == nil {
				t.Fatalf("unexpected synthetic result: %+v", got)
			}
			wantKinds := []string{}
			if tc.kind != "" {
				wantKinds = append(wantKinds, tc.kind)
			}
			if !*got.Assessment.Readable || *got.Assessment.ContainsSensitive != (tc.kind != "") || !slices.Equal(got.Assessment.Kinds, wantKinds) {
				t.Fatalf("incorrect assessment on synthetic page: %+v", got.Assessment)
			}
		})
	}
}
