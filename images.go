package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxImageBytes = 32 << 20

func mimeType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return "application/pdf"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".jp2", ".jpx", ".j2k":
		return "image/jp2"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	}
	return ""
}

func imageFormat(file string) string {
	switch mimeType(file) {
	case "application/pdf":
		return "pdf"
	case "image/jpeg":
		return "jpeg"
	case "image/tiff":
		return "tiff"
	case "image/jp2":
		return "jp2"
	default:
		return strings.TrimPrefix(mimeType(file), "image/")
	}
}

// Stream input to the container so Docker also works with a remote daemon.
// Only fixed shell programs run; paths and document contents are never shell code.
func dockerImage(ctx context.Context, file string, o options, script string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	input, err := os.Open(file) // #nosec G304 -- Input is an explicitly selected local document.
	if err != nil {
		return nil, err
	}
	defer input.Close()
	name := "hawkeye-" + strings.ToLower(rand.Text())
	argv := []string{"run", "--rm", "--name", name, "-i", "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory=2g", "--cpus=2", "--pids-limit=128", "--tmpfs", "/tmp:rw,noexec,nosuid,size=1g", "--entrypoint=/bin/sh", o.image, "-c", script, "hawkeye"}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "docker", argv...) // #nosec G204 -- Fixed shell programs; variable arguments are passed separately.
	defer func() {
		if ctx.Err() != nil {
			// Killing the Docker client alone can leave its container running.
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run() // #nosec G204 -- Name is generated locally, without shell interpretation.
		}
	}()
	cmd.Stdin = input
	cmd.WaitDelay = 5 * time.Second
	var output limitedBuffer
	cmd.Stdout = &output
	// Conversion diagnostics may echo document text; return only the exit error.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker conversion failed: %w", err)
	}
	if output.exceeded {
		return nil, errors.New("converted image exceeds 32 MiB")
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxImageBytes {
		b.exceeded = true
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func pageCount(ctx context.Context, file string, o options) (int, error) {
	output, err := dockerImage(ctx, file, o,
		`set -eu; cat > /tmp/input; exec magick identify -ping -format '%n\n' "$1:/tmp/input"`, imageFormat(file))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return 0, errors.New("no pages found")
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 1 || n != len(fields) {
		return 0, errors.New("invalid page count")
	}
	for _, field := range fields {
		if field != fields[0] {
			return 0, errors.New("inconsistent page count")
		}
	}
	return n, nil
}

func prepareImage(ctx context.Context, file string, page, count int, o options) ([]byte, error) {
	// Single images can go straight to the optimization service with their MIME
	// type. Multipage documents must first be split to avoid first-page-only scans.
	if o.houdiniURL != "" && count == 1 && mimeType(file) != "application/pdf" {
		f, err := os.Open(file) // #nosec G304 -- Input is an explicitly selected local document.
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return optimize(ctx, f, mimeType(file), o)
	}
	data, err := dockerImage(ctx, file, o,
		`set -eu; cat > /tmp/input; if [ "$4" = 0 ]; then set -- "$1" "$2" "$3" 100%; else set -- "$1" "$2" "$3" "${4}x${4}>"; fi; exec magick -density "$3" "$1:/tmp/input[$2]" -auto-orient -background white -alpha remove -alpha off -resize "$4" -strip png:-`,
		imageFormat(file), strconv.Itoa(page-1), strconv.Itoa(o.dpi), strconv.Itoa(o.maxEdge))
	if err != nil {
		return nil, err
	}
	if o.houdiniURL != "" {
		return optimize(ctx, bytes.NewReader(data), "image/png", o)
	}
	return validateImage(data)
}

func optimize(ctx context.Context, input io.Reader, mime string, o options) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.houdiniURL, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mime)
	req.Header.Set("Accept", "image/png, image/jpeg")
	client := &http.Client{Timeout: o.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image service returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	return validateImage(data)
}

func validateImage(data []byte) ([]byte, error) {
	if len(data) > maxImageBytes {
		return nil, errors.New("image exceeds 32 MiB")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 {
		return nil, errors.New("expected a PNG or JPEG image")
	}
	return data, nil
}
