// Package memory is the in-memory driver: for tests and demos, with nothing
// persisted (R-DRV-01).
package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Driver keeps records in maps.
type Driver struct {
	mu   sync.RWMutex
	cols map[string]map[string]store.Record
	// byDoc indexes each collection's record IDs by document, so loading one
	// document doesn't scan the whole library.
	byDoc map[string]map[string]map[string]struct{}
}

// New makes an empty memory driver. The caller logs the startup warning that
// nothing will persist, since only the caller has the logger.
func New() *Driver {
	return &Driver{cols: map[string]map[string]store.Record{}, byDoc: map[string]map[string]map[string]struct{}{}}
}

// index and unindex keep byDoc in step with a collection. Call with mu held.
func (d *Driver) index(col string, r store.Record) {
	docs := d.byDoc[col]
	if docs == nil {
		docs = map[string]map[string]struct{}{}
		d.byDoc[col] = docs
	}
	ids := docs[r.Doc]
	if ids == nil {
		ids = map[string]struct{}{}
		docs[r.Doc] = ids
	}
	ids[r.ID] = struct{}{}
}

func (d *Driver) unindex(col string, r store.Record) {
	if ids := d.byDoc[col][r.Doc]; ids != nil {
		delete(ids, r.ID)
		if len(ids) == 0 {
			delete(d.byDoc[col], r.Doc)
		}
	}
}

// Name implements store.Driver.
func (d *Driver) Name() string { return "memory" }

// Caps implements store.Driver. Search is a simple scan, which is enough for
// tests and demos.
func (d *Driver) Caps() store.Caps {
	return store.Caps{FullText: true, Vector: true, NativeWholeDoc: true}
}

// Ensure implements store.Driver.
func (d *Driver) Ensure(_ context.Context, cols []store.CollectionSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range cols {
		if _, ok := d.cols[c.Name]; !ok {
			d.cols[c.Name] = map[string]store.Record{}
		}
	}
	return nil
}

func (d *Driver) col(name string) (map[string]store.Record, error) {
	c, ok := d.cols[name]
	if !ok {
		return nil, fmt.Errorf("memory: no collection %q", name)
	}
	return c, nil
}

// Put implements store.Driver.
func (d *Driver) Put(_ context.Context, col string, recs []store.Record) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.col(col)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.ID == "" {
			return fmt.Errorf("memory: record with no ID")
		}
		if old, ok := c[r.ID]; ok {
			d.unindex(col, old)
		}
		c[r.ID] = r.Clone()
		d.index(col, r)
	}
	return nil
}

// Get implements store.Driver.
func (d *Driver) Get(_ context.Context, col string, ids []string) ([]store.Record, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, err := d.col(col)
	if err != nil {
		return nil, err
	}
	out := make([]store.Record, 0, len(ids))
	for _, id := range ids {
		if r, ok := c[id]; ok {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

// Find implements store.Driver. The cursor is the last ID returned; pages walk
// the IDs in byte order.
func (d *Driver) Find(_ context.Context, col string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	if limit <= 0 {
		return nil, "", fmt.Errorf("memory: limit must be positive")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, err := d.col(col)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	if f.Doc != "" {
		for id := range d.byDoc[col][f.Doc] {
			if id > after && f.Match(c[id]) {
				ids = append(ids, id)
			}
		}
	} else {
		ids = make([]string, 0, len(c))
		for id, r := range c {
			if id > after && f.Match(r) {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
		next = ids[limit-1]
	}
	out := make([]store.Record, len(ids))
	for i, id := range ids {
		out[i] = c[id].Clone()
	}
	return out, next, nil
}

// Delete implements store.Driver.
func (d *Driver) Delete(_ context.Context, col string, ids []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.col(col)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if r, ok := c[id]; ok {
			d.unindex(col, r)
			delete(c, id)
		}
	}
	return nil
}

// DeleteWhere implements store.Driver.
func (d *Driver) DeleteWhere(_ context.Context, col string, f store.Filter) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.col(col)
	if err != nil {
		return err
	}
	for id, r := range c {
		if f.Match(r) {
			d.unindex(col, r)
			delete(c, id)
		}
	}
	return nil
}

// Tx implements store.Driver. The memory driver has no transactions, so fn
// runs directly.
func (d *Driver) Tx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// SearchText implements store.Driver with a case-insensitive scan: each query
// word found in a record's text scores one.
func (d *Driver) SearchText(_ context.Context, col, q string, f store.Filter, k int) ([]store.Hit, error) {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return nil, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, err := d.col(col)
	if err != nil {
		return nil, err
	}
	var hits []store.Hit
	for _, r := range c {
		if !f.Match(r) {
			continue
		}
		text := strings.ToLower(r.Text)
		score := 0.0
		for _, w := range words {
			if strings.Contains(text, w) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, store.Hit{Record: r.Clone(), Score: score})
		}
	}
	return topK(hits, k), nil
}

// SearchVector implements store.Driver with cosine similarity over every
// native vector of the model.
func (d *Driver) SearchVector(_ context.Context, col, model string, v []float32, f store.Filter, k int) ([]store.Hit, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, err := d.col(col)
	if err != nil {
		return nil, err
	}
	var hits []store.Hit
	for _, r := range c {
		if r.VectorModel != model || len(r.Vector) != len(v) || !f.Match(r) {
			continue
		}
		hits = append(hits, store.Hit{Record: r.Clone(), Score: cosine(v, r.Vector)})
	}
	return topK(hits, k), nil
}

// topK sorts hits best first, breaking ties by ID so results are stable, and
// keeps k of them.
func topK(hits []store.Hit, k int) []store.Hit {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Record.ID < hits[j].Record.ID
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Ping implements store.Driver.
func (d *Driver) Ping(context.Context) error { return nil }

// Close implements store.Driver.
func (d *Driver) Close() error { return nil }
