package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type documentCheckpoint struct {
	count int
	done  map[int]bool
}

type reportCheckpoint struct {
	file       *os.File
	documents  map[string]*documentCheckpoint
	validBytes int64
	partial    bool
	newline    bool
}

func (c *reportCheckpoint) pageDone(file string, page int) bool {
	d := c.documents[file]
	return d != nil && d.done[page]
}

func (c *reportCheckpoint) fileDone(file string) bool {
	d := c.documents[file]
	if d == nil {
		return false
	}
	count := d.count
	if count == 0 {
		// Older reports lack page_count. Enumerate potentially multipage formats.
		switch mimeType(file) {
		case "image/jpeg", "image/png", "image/jp2", "image/bmp":
			count = 1
		default:
			return false
		}
	}
	if len(d.done) < count {
		return false
	}
	for page := 1; page <= count; page++ {
		if !d.done[page] {
			return false
		}
	}
	return true
}

func loadReport(path string) (*reportCheckpoint, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &reportCheckpoint{documents: make(map[string]*documentCheckpoint)}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("report must be a regular file, not a symlink or directory")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600) // #nosec G304 -- Explicit report selected by the user.
	if err != nil {
		return nil, err
	}
	c, err := readReport(f, nil)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	c.file = f
	return c, nil
}

// Retain only completion metadata, not OCR. The viewer can receive each record
// without opening the report for writing. Only the scanner repairs a torn tail.
func readReport(input io.Reader, onRecord func(result)) (*reportCheckpoint, error) {
	c := &reportCheckpoint{documents: make(map[string]*documentCheckpoint)}
	reader := bufio.NewReader(input)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("read report: %w", readErr)
		}
		if len(bytes.TrimSpace(line)) != 0 {
			var r result
			dec := json.NewDecoder(bytes.NewReader(line))
			err := dec.Decode(&r)
			if readErr == io.EOF && errors.Is(err, io.ErrUnexpectedEOF) {
				c.partial = true
				break
			}
			if err == nil {
				var extra any
				if dec.Decode(&extra) != io.EOF {
					err = errors.New("expected one JSON object per line")
				}
			}
			if err != nil || !filepath.IsAbs(r.File) || r.Page < 0 || r.PageCount < 0 || (r.PageCount > 0 && r.Page > r.PageCount) ||
				(r.Status != "review" && r.Status != "no_findings" && r.Status != "error") || (r.Page == 0 && r.Status != "error") {
				// Decoder errors can quote sensitive text. Report only its location.
				return nil, fmt.Errorf("invalid report record on line %d; report left unchanged", lineNumber)
			}
			file := filepath.Clean(r.File)
			r.File = file
			d := c.documents[file]
			if d == nil {
				d = &documentCheckpoint{done: make(map[int]bool)}
				c.documents[file] = d
			}
			if r.PageCount > 0 {
				if d.count != 0 && d.count != r.PageCount {
					return nil, fmt.Errorf("inconsistent page counts on report line %d; use a new report for changed documents", lineNumber)
				}
				d.count = r.PageCount
			}
			if r.Page > 0 {
				d.done[r.Page] = r.Status != "error" && r.Error == ""
			} else {
				// A later enumeration failure invalidates older page completions.
				d.done = make(map[int]bool)
			}
			if onRecord != nil {
				onRecord(r)
			}
		}
		c.validBytes += int64(len(line))
		c.newline = len(line) > 0 && line[len(line)-1] != '\n'
		if readErr == io.EOF {
			break
		}
	}
	return c, nil
}

func (c *reportCheckpoint) prepareAppend(path string) error {
	if c.file == nil {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) // #nosec G304 -- Explicit report selected by the user.
		if err != nil {
			return err
		}
		c.file = f
		return nil
	}
	if c.partial {
		if err := c.file.Truncate(c.validBytes); err != nil {
			return err
		}
	}
	if c.newline {
		if _, err := c.file.WriteString("\n"); err != nil {
			return err
		}
	}
	if c.partial || c.newline {
		return c.file.Sync()
	}
	return nil
}
