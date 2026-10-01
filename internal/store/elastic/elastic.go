// Package elastic stores records in Elasticsearch indices (R-DRV-04), over
// its REST API. Each record's _id is its ID; the envelope fields are
// keywords; node is kept in _source but not indexed; text is analysed with
// the screenjson_search analyzer from screenjson-schema; a level with a
// native vector gets a dense_vector field. Writes wait for a refresh by
// default, so a write is visible to the next read; loads page with
// search_after.
package elastic

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// searchSettings is screenjson-schema's elasticsearch/search.json. A drift
// test keeps it equal to the schema repository's copy.
//
//go:embed search.json
var searchSettings []byte

// Driver is an Elasticsearch cluster.
type Driver struct {
	base    string
	env     string
	refresh string
	http    *http.Client
	mu      sync.RWMutex
	vectors map[string]int
}

// Open connects to the cluster at url. refresh is the refresh policy for
// writes: wait_for (default), true or false.
func Open(ctx context.Context, rawURL, envelope, refresh string) (*Driver, error) {
	if envelope == "" {
		envelope = "sj"
	}
	if refresh == "" {
		refresh = "wait_for"
	}
	d := &Driver{base: strings.TrimRight(rawURL, "/"), env: envelope, refresh: refresh,
		http: &http.Client{Timeout: 60 * time.Second}, vectors: map[string]int{}}
	if err := d.Ping(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Name implements store.Driver.
func (d *Driver) Name() string { return "elastic" }

// Caps implements store.Driver.
func (d *Driver) Caps() store.Caps {
	return store.Caps{FullText: true, Vector: true, NativeWholeDoc: true, MaxRecordBytes: 100 << 20}
}

// index is a collection's index name: Elastic wants lowercase.
func index(col string) string { return strings.ToLower(col) }

// call sends a request and decodes a JSON answer into out.
func (d *Driver) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
		ct = "application/x-ndjson"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		return 0, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", ct)
	}
	res, err := d.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("elastic: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotFound {
		return res.StatusCode, fmt.Errorf("elastic: %s %s: %s: %s", method, path, res.Status, truncate(raw))
	}
	if out != nil && len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(out); err != nil {
			return res.StatusCode, fmt.Errorf("elastic: decode %s: %w", path, err)
		}
	}
	return res.StatusCode, nil
}

func truncate(b []byte) string {
	if len(b) > 500 {
		return string(b[:500]) + "…"
	}
	return string(b)
}

// Ensure implements store.Driver: creates each index with the analyzer and
// mappings if it doesn't exist.
func (d *Driver) Ensure(ctx context.Context, cols []store.CollectionSpec) error {
	var settings struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(searchSettings, &settings); err != nil {
		return fmt.Errorf("elastic: search settings: %w", err)
	}
	for _, c := range cols {
		idx := index(c.Name)
		if c.Vector != nil {
			d.mu.Lock()
			d.vectors[idx] = c.Vector.Dimensions
			d.mu.Unlock()
		}
		status, err := d.call(ctx, http.MethodHead, "/"+idx, nil, nil)
		if err != nil {
			return err
		}
		if status == http.StatusOK {
			continue
		}
		kw := map[string]any{"type": "keyword"}
		long := map[string]any{"type": "long"}
		props := map[string]any{
			d.env: map[string]any{"properties": map[string]any{
				"id": kw, "doc": kw, "parent": kw, "kind": kw, "type": kw, "order": kw,
				"rev": long, "text_rev": long, "crev": long, "seq": long,
				"updated": map[string]any{"type": "date"}, "layout": kw, "vector_model": kw,
				"vector_meta": map[string]any{"type": "object", "enabled": false},
			}},
			"node": map[string]any{"type": "object", "enabled": false},
			"text": map[string]any{"type": "text", "analyzer": "screenjson_index", "search_analyzer": "screenjson_search"},
		}
		if c.Vector != nil {
			props["vector"] = map[string]any{"type": "dense_vector", "dims": c.Vector.Dimensions, "index": true, "similarity": "cosine"}
		}
		var set map[string]any
		if err := json.Unmarshal(settings.Settings, &set); err != nil {
			return fmt.Errorf("elastic: search settings: %w", err)
		}
		// Elastic 9 leaves dense vectors out of _source by default; they are
		// part of the record and must come back.
		set["index.mapping.exclude_source_vectors"] = false
		body := map[string]any{"settings": set, "mappings": map[string]any{"dynamic": "strict", "properties": props}}
		if _, err := d.call(ctx, http.MethodPut, "/"+idx, body, nil); err != nil {
			if strings.Contains(err.Error(), "resource_already_exists_exception") {
				continue
			}
			return fmt.Errorf("elastic: ensure %s: %w", c.Name, err)
		}
	}
	return nil
}

// Tx implements store.Driver; Elastic has no transactions.
func (d *Driver) Tx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

func (d *Driver) source(r store.Record) map[string]any {
	env := map[string]any{
		"id": r.ID, "doc": r.Doc, "parent": r.Parent, "kind": r.Kind, "type": r.Type, "order": r.Order,
		"rev": r.Rev, "text_rev": r.TextRev, "crev": r.CRev, "seq": r.Seq, "layout": r.Layout,
	}
	if !r.Updated.IsZero() {
		env["updated"] = r.Updated.UTC().Format(time.RFC3339Nano)
	}
	if r.VectorModel != "" {
		env["vector_model"] = r.VectorModel
	}
	if len(r.VectorMeta) > 0 {
		env["vector_meta"] = json.RawMessage(r.VectorMeta)
	}
	src := map[string]any{d.env: env, "text": r.Text}
	if len(r.Node) > 0 {
		src["node"] = json.RawMessage(r.Node)
	}
	if r.Vector != nil {
		src["vector"] = r.Vector
	}
	return src
}

// hit is one document as Elastic returns it.
type hit struct {
	ID     string                     `json:"_id"`
	Found  *bool                      `json:"found"`
	Score  *float64                   `json:"_score"`
	Source map[string]json.RawMessage `json:"_source"`
	Sort   []any                      `json:"sort"`
}

func (d *Driver) record(h hit) (store.Record, error) {
	r := store.Record{ID: h.ID}
	var env struct {
		Doc, Parent, Kind, Type, Order, Layout string
		Rev, TextRev, CRev, Seq                uint64
		Updated                                string
		VectorModel                            string          `json:"vector_model"`
		VectorMeta                             json.RawMessage `json:"vector_meta"`
	}
	if raw, ok := h.Source[d.env]; ok {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return r, err
		}
		str := func(k string) string {
			var s string
			_ = json.Unmarshal(m[k], &s)
			return s
		}
		num := func(k string) uint64 {
			var n uint64
			_ = json.Unmarshal(m[k], &n)
			return n
		}
		env.Doc, env.Parent, env.Kind, env.Type, env.Order, env.Layout = str("doc"), str("parent"), str("kind"), str("type"), str("order"), str("layout")
		env.Rev, env.TextRev, env.CRev, env.Seq = num("rev"), num("text_rev"), num("crev"), num("seq")
		env.Updated, env.VectorModel, env.VectorMeta = str("updated"), str("vector_model"), m["vector_meta"]
	}
	r.Doc, r.Parent, r.Kind, r.Type, r.Order, r.Layout = env.Doc, env.Parent, env.Kind, env.Type, env.Order, env.Layout
	r.Rev, r.TextRev, r.CRev, r.Seq, r.VectorModel = env.Rev, env.TextRev, env.CRev, env.Seq, env.VectorModel
	if len(env.VectorMeta) > 0 && string(env.VectorMeta) != "null" {
		r.VectorMeta = env.VectorMeta
	}
	if env.Updated != "" {
		t, err := time.Parse(time.RFC3339Nano, env.Updated)
		if err != nil {
			return r, err
		}
		r.Updated = t.UTC()
	}
	if raw, ok := h.Source["node"]; ok {
		r.Node = raw
	}
	if raw, ok := h.Source["text"]; ok {
		_ = json.Unmarshal(raw, &r.Text)
	}
	if raw, ok := h.Source["vector"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &r.Vector); err != nil {
			return r, err
		}
	}
	return r, nil
}

func (d *Driver) bulk(ctx context.Context, body []byte, refresh string) error {
	var res struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if _, err := d.call(ctx, http.MethodPost, "/_bulk?refresh="+url.QueryEscape(refresh), body, &res); err != nil {
		return err
	}
	if res.Errors {
		for _, item := range res.Items {
			for op, r := range item {
				if len(r.Error) > 0 && !(op == "delete" && r.Status == http.StatusNotFound) {
					return fmt.Errorf("elastic: bulk %s: %s", op, r.Error)
				}
			}
		}
	}
	return nil
}

// Put implements store.Driver with one bulk request.
func (d *Driver) Put(ctx context.Context, col string, recs []store.Record) error {
	if len(recs) == 0 {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(map[string]any{"index": map[string]any{"_index": index(col), "_id": r.ID}}); err != nil {
			return err
		}
		if err := enc.Encode(d.source(r)); err != nil {
			return fmt.Errorf("elastic: encode %s: %w", r.ID, err)
		}
	}
	if err := d.bulk(ctx, b.Bytes(), d.refresh); err != nil {
		return fmt.Errorf("elastic: put into %s: %w", col, err)
	}
	return nil
}

// Get implements store.Driver.
func (d *Driver) Get(ctx context.Context, col string, ids []string) ([]store.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var res struct {
		Docs []hit `json:"docs"`
	}
	if _, err := d.call(ctx, http.MethodPost, "/"+index(col)+"/_mget", map[string]any{"ids": ids}, &res); err != nil {
		return nil, fmt.Errorf("elastic: get from %s: %w", col, err)
	}
	var out []store.Record
	for _, h := range res.Docs {
		if h.Found == nil || !*h.Found {
			continue
		}
		r, err := d.record(h)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (d *Driver) filters(f store.Filter) []any {
	var out []any
	term := func(field, v string) {
		if v != "" {
			out = append(out, map[string]any{"term": map[string]any{d.env + "." + field: v}})
		}
	}
	term("doc", f.Doc)
	term("parent", f.Parent)
	term("kind", f.Kind)
	term("type", f.Type)
	if len(f.IDs) > 0 {
		out = append(out, map[string]any{"terms": map[string]any{d.env + ".id": f.IDs}})
	}
	return out
}

func (d *Driver) search(ctx context.Context, col string, body map[string]any) ([]hit, error) {
	var res struct {
		Hits struct {
			Hits []hit `json:"hits"`
		} `json:"hits"`
	}
	status, err := d.call(ctx, http.MethodPost, "/"+index(col)+"/_search", body, &res)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("elastic: no index %s", index(col))
	}
	return res.Hits.Hits, nil
}

// Find implements store.Driver, paging with search_after on the ID.
func (d *Driver) Find(ctx context.Context, col string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("elastic: limit must be positive")
	}
	body := map[string]any{
		"size":             limit + 1,
		"query":            map[string]any{"bool": map[string]any{"filter": d.filters(f)}},
		"sort":             []any{map[string]any{d.env + ".id": "asc"}},
		"track_total_hits": false,
	}
	if after != "" {
		body["search_after"] = []any{after}
	}
	hits, err := d.search(ctx, col, body)
	if err != nil {
		return nil, "", fmt.Errorf("elastic: find in %s: %w", col, err)
	}
	out := make([]store.Record, 0, len(hits))
	for _, h := range hits {
		r, err := d.record(h)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}

// Delete implements store.Driver.
func (d *Driver) Delete(ctx context.Context, col string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, id := range ids {
		_ = enc.Encode(map[string]any{"delete": map[string]any{"_index": index(col), "_id": id}})
	}
	if err := d.bulk(ctx, b.Bytes(), d.refresh); err != nil {
		return fmt.Errorf("elastic: delete from %s: %w", col, err)
	}
	return nil
}

// DeleteWhere implements store.Driver.
func (d *Driver) DeleteWhere(ctx context.Context, col string, f store.Filter) error {
	body := map[string]any{"query": map[string]any{"bool": map[string]any{"filter": d.filters(f)}}}
	refresh := "true"
	if d.refresh == "false" {
		refresh = "false"
	}
	if _, err := d.call(ctx, http.MethodPost, "/"+index(col)+"/_delete_by_query?conflicts=proceed&refresh="+refresh, body, nil); err != nil {
		return fmt.Errorf("elastic: delete from %s: %w", col, err)
	}
	return nil
}

func (d *Driver) hits(hs []hit) ([]store.Hit, error) {
	out := make([]store.Hit, 0, len(hs))
	for _, h := range hs {
		r, err := d.record(h)
		if err != nil {
			return nil, err
		}
		score := 0.0
		if h.Score != nil {
			score = *h.Score
		}
		out = append(out, store.Hit{Record: r, Score: score})
	}
	return out, nil
}

// SearchText implements store.Driver with a match query on text.
func (d *Driver) SearchText(ctx context.Context, col, q string, f store.Filter, k int) ([]store.Hit, error) {
	body := map[string]any{
		"size": k,
		"query": map[string]any{"bool": map[string]any{
			"must":   []any{map[string]any{"match": map[string]any{"text": q}}},
			"filter": d.filters(f),
		}},
	}
	hs, err := d.search(ctx, col, body)
	if err != nil {
		return nil, fmt.Errorf("elastic: text search in %s: %w", col, err)
	}
	return d.hits(hs)
}

// SearchVector implements store.Driver with a kNN query on the native vector.
func (d *Driver) SearchVector(ctx context.Context, col, model string, v []float32, f store.Filter, k int) ([]store.Hit, error) {
	d.mu.RLock()
	_, ok := d.vectors[index(col)]
	d.mu.RUnlock()
	if !ok {
		return nil, store.ErrUnsupported
	}
	filters := append(d.filters(f), map[string]any{"term": map[string]any{d.env + ".vector_model": model}})
	body := map[string]any{
		"size": k,
		"knn": map[string]any{
			"field": "vector", "query_vector": v, "k": k, "num_candidates": max(100, k*10),
			"filter": map[string]any{"bool": map[string]any{"filter": filters}},
		},
	}
	hs, err := d.search(ctx, col, body)
	if err != nil {
		return nil, fmt.Errorf("elastic: vector search in %s: %w", col, err)
	}
	return d.hits(hs)
}

// Ping implements store.Driver.
func (d *Driver) Ping(ctx context.Context) error {
	var info map[string]any
	if _, err := d.call(ctx, http.MethodGet, "/", nil, &info); err != nil {
		return err
	}
	return nil
}

// Close implements store.Driver.
func (d *Driver) Close() error {
	d.http.CloseIdleConnections()
	return nil
}
