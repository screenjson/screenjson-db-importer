// Package dbimporter provides streaming imports using the server storage engine.
package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/screenjson/screenjson-db-importer/internal/blob"
	"github.com/screenjson/screenjson-db-importer/internal/config"
	"github.com/screenjson/screenjson-db-importer/internal/docs"
)

const maxBytes = 32 << 20

type result struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	DocID  string `json:"document_id,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type candidate struct{ source, sum string }

// Options configures one offline import run.
type Options struct {
	ConfigFile  string
	Source      string
	Workers     int
	Manifest    string
	RetryFailed bool
	Output      io.Writer
	ErrorOutput io.Writer
}

// Run preflights and imports JSON sources through the server document manager.
func Run(ctx context.Context, o Options) error {
	src, cfgFile, workers, manifestPath, retry := o.Source, o.ConfigFile, o.Workers, o.Manifest, o.RetryFailed
	if workers < 1 || workers > 256 {
		return errors.New("workers must be between 1 and 256")
	}
	if src == "" {
		return errors.New("source is required")
	}
	if manifestPath == "" {
		manifestPath = "screenjson-import.jsonl"
	}
	out, errOut := o.Output, o.ErrorOutput
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	cfg, err := config.Load(config.Options{File: cfgFile})
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	blobs := blob.New(cfg.Blob)
	sources, err := enumerate(ctx, src, blobs)
	if err != nil {
		return fmt.Errorf("enumerate %q: %w", src, err)
	}
	if len(sources) == 0 {
		return fmt.Errorf("no .json files found at %q", src)
	}
	mf, done, err := openManifest(manifestPath)
	if err != nil {
		return err
	}
	defer mf.Close()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// New owns the same state lock and server lease as direct administrative
	// operations. It refuses an active server before any document is written.
	a, err := openOffline(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("open import environment (stop the server first): %w", err)
	}
	defer a.Close()

	valid := make([]candidate, 0, len(sources))
	failed := 0
	for i, source := range sources {
		body, readErr := readSource(ctx, source, blobs)
		if readErr != nil {
			r := result{Source: source, Status: "failed", Error: readErr.Error()}
			if e := record(mf, r); e != nil {
				return e
			}
			failed++
			fmt.Fprintf(errOut, "INVALID %s: %s\n", source, readErr)
			continue
		}
		sum := digest(body)
		if prev, ok := done[source]; ok && prev.SHA256 == sum && prev.Status == "done" {
			fmt.Fprintf(out, "SKIP %s (already imported, sha256 %s)\n", source, sum[:12])
			continue
		}
		if prev, ok := done[source]; ok && prev.SHA256 == sum && prev.Status == "failed" && !retry {
			fmt.Fprintf(out, "SKIP %s (failed previously; pass --retry-failed)\n", source)
			failed++
			continue
		}
		if err := a.Manager.ValidateImport(body); err != nil {
			r := result{Source: source, SHA256: sum, Status: "failed", Error: err.Error()}
			if e := record(mf, r); e != nil {
				return e
			}
			done[source] = r
			failed++
			fmt.Fprintf(errOut, "INVALID %s: %s\n", source, err)
			continue
		}
		valid = append(valid, candidate{source: source, sum: sum})
		if (i+1)%100 == 0 {
			fmt.Fprintf(out, "preflight %d/%d; valid %d, failed %d\n", i+1, len(sources), len(valid), failed)
		}
	}
	jobs := make(chan candidate)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				body, e := readSource(ctx, c.source, blobs)
				var r result
				if e == nil && digest(body) != c.sum {
					e = errors.New("source changed after preflight; refusing to import changed content")
				}
				if e == nil {
					var out *docs.Response
					out, e = a.Manager.Create(ctx, &docs.Request{Actor: "db-importer", Body: body})
					if e == nil {
						if m, ok := out.Body.(map[string]any); ok {
							r.DocID, _ = m["id"].(string)
						}
					}
				}
				r.Source, r.SHA256 = c.source, c.sum
				if e != nil {
					r.Status, r.Error = "failed", e.Error()
				} else {
					r.Status = "done"
				}
				mu.Lock()
				if me := record(mf, r); me != nil {
					r.Status, r.Error = "failed", "manifest write failed: "+me.Error()
				}
				if r.Status == "failed" {
					failed++
				}
				fmt.Fprintf(out, "%s %s %s\n", strings.ToUpper(r.Status), c.source, r.DocID)
				mu.Unlock()
			}
		}()
	}
	for _, c := range valid {
		jobs <- c
	}
	close(jobs)
	wg.Wait()
	if failed != 0 {
		return fmt.Errorf("import finished: %d source(s), %d failed; see %s", len(sources), failed, manifestPath)
	}
	fmt.Fprintf(out, "imported %d document(s); manifest %s\n", len(valid), manifestPath)
	return nil
}

func enumerate(ctx context.Context, src string, b *blob.Blobs) ([]string, error) {
	if strings.HasPrefix(src, "s3://") || strings.HasPrefix(src, "azure://") || strings.HasPrefix(src, "file://") {
		items, err := b.List(ctx, src)
		if err != nil {
			return nil, err
		}
		out := items[:0]
		for _, s := range items {
			if strings.EqualFold(filepath.Ext(s), ".json") {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	var paths []string
	if strings.ContainsAny(src, "*?[") {
		paths, _ = filepath.Glob(src)
		if paths == nil {
			if _, err := filepath.Match(src, ""); err != nil {
				return nil, err
			}
		}
	} else {
		st, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("stat source: %w", err)
		}
		if st.IsDir() {
			err = filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
				if e != nil {
					return fmt.Errorf("walk %s: %w", p, e)
				}
				if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".json") {
					paths = append(paths, p)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			paths = []string{src}
		}
	}
	out := paths[:0]
	for _, p := range paths {
		if strings.EqualFold(filepath.Ext(p), ".json") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func readSource(ctx context.Context, source string, b *blob.Blobs) ([]byte, error) {
	var body []byte
	var err error
	if strings.HasPrefix(source, "s3://") || strings.HasPrefix(source, "azure://") || strings.HasPrefix(source, "file://") {
		body, err = b.Get(ctx, source)
	} else {
		var f *os.File
		f, err = os.Open(source)
		if err == nil {
			defer f.Close()
			body, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("file is empty")
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d MiB limit", maxBytes>>20)
	}
	if !json.Valid(body) {
		return nil, errors.New("malformed JSON (check file encoding and syntax)")
	}
	return body, nil
}

func digest(body []byte) string { h := sha256.Sum256(body); return hex.EncodeToString(h[:]) }
func openManifest(path string) (*os.File, map[string]result, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open manifest %s: %w", path, err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	done := map[string]result{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var r result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("manifest %s contains invalid JSONL: %w", path, err)
		}
		done[r.Source] = r
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	_, err = f.Seek(0, io.SeekEnd)
	return f, done, err
}
func record(f *os.File, r result) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
