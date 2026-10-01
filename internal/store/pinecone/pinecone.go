// Package pinecone stores server records in namespaces of a Pinecone dense
// vector index. Record envelopes and ScreenJSON stay in metadata as JSON text.
package pinecone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

const apiVersion = "2025-04"

type Driver struct {
	base, key, namespace string
	http                 *http.Client
	mu                   sync.RWMutex
	dims                 int
	cols                 map[string]column
}
type column struct {
	namespace string
	vector    bool
}

// Open connects to a Pinecone index by its data-plane host and API key.
func Open(ctx context.Context, host, key, namespace string) (*Driver, error) {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(key) == "" {
		return nil, errors.New("pinecone: index host and API key are required")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	d := &Driver{base: strings.TrimRight(host, "/"), key: key, namespace: strings.Trim(namespace, "/"), http: &http.Client{Timeout: 60 * time.Second}, cols: map[string]column{}}
	if err := d.Ping(ctx); err != nil {
		return nil, err
	}
	return d, nil
}
func (d *Driver) Name() string     { return "pinecone" }
func (d *Driver) Caps() store.Caps { return store.Caps{Vector: true, MaxRecordBytes: 40_000} }
func (d *Driver) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Api-Key", d.key)
	req.Header.Set("X-Pinecone-Api-Version", apiVersion)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("pinecone %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if len(raw) > 500 {
			raw = raw[:500]
		}
		return fmt.Errorf("pinecone %s %s: HTTP %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("pinecone decode response: %w", err)
		}
	}
	return nil
}
func (d *Driver) Ping(ctx context.Context) error {
	var stats struct {
		Dimension int `json:"dimension"`
	}
	if err := d.call(ctx, http.MethodPost, "/describe_index_stats", map[string]any{}, &stats); err != nil {
		return fmt.Errorf("pinecone ping: %w", err)
	}
	if stats.Dimension < 1 {
		return fmt.Errorf("pinecone: index returned invalid dimension %d", stats.Dimension)
	}
	d.mu.Lock()
	d.dims = stats.Dimension
	d.mu.Unlock()
	return nil
}
func (d *Driver) Close() error { return nil }
func (d *Driver) Ensure(ctx context.Context, specs []store.CollectionSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range specs {
		if s.Vector != nil && s.Vector.Dimensions != d.dims {
			return fmt.Errorf("pinecone: collection %s declares vector dimension %d but index dimension is %d", s.Name, s.Vector.Dimensions, d.dims)
		}
		ns := d.namespace
		if ns != "" {
			ns += "_"
		}
		ns += s.Name
		if len(ns) > 100 {
			return fmt.Errorf("pinecone: namespace for collection %s exceeds 100 characters", s.Name)
		}
		d.cols[s.Name] = column{namespace: ns, vector: s.Vector != nil}
	}
	return nil
}
func (d *Driver) col(name string) (column, error) {
	d.mu.RLock()
	c, ok := d.cols[name]
	d.mu.RUnlock()
	if !ok {
		return column{}, fmt.Errorf("pinecone: collection %q has not been ensured", name)
	}
	return c, nil
}
func (d *Driver) Tx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func metadata(r store.Record) map[string]any {
	m := map[string]any{
		"sj_id":  r.ID,
		"sj_doc": r.Doc, "sj_parent": r.Parent, "sj_kind": r.Kind, "sj_type": r.Type, "sj_order": r.Order,
		"sj_rev": int64(r.Rev), "sj_text_rev": int64(r.TextRev), "sj_crev": int64(r.CRev), "sj_seq": int64(r.Seq),
		"sj_layout": r.Layout, "sj_has_vector": r.Vector != nil, "sj_vector_model": r.VectorModel,
		"sj_vector_meta": string(r.VectorMeta), "sj_node": string(r.Node), "sj_text": r.Text,
	}
	if !r.Updated.IsZero() {
		m["sj_updated"] = r.Updated.UTC().Format(time.RFC3339Nano)
	}
	return m
}

type vectorInput struct {
	ID       string         `json:"id"`
	Values   []float32      `json:"values"`
	Metadata map[string]any `json:"metadata"`
}

func (d *Driver) Put(ctx context.Context, name string, recs []store.Record) error {
	c, err := d.col(name)
	if err != nil {
		return err
	}
	dim := d.dimension()
	for from := 0; from < len(recs); from += 100 {
		end := min(from+100, len(recs))
		vs := make([]vectorInput, 0, end-from)
		for _, r := range recs[from:end] {
			v := r.Vector
			if v != nil && len(v) != dim {
				return fmt.Errorf("pinecone: record %s vector has %d dimensions, index has %d", r.ID, len(v), dim)
			}
			if v == nil {
				v = make([]float32, dim)
			}
			vs = append(vs, vectorInput{ID: r.ID, Values: v, Metadata: metadata(r)})
		}
		path := "/vectors/upsert"
		b, _ := json.Marshal(map[string]any{"vectors": vs, "namespace": c.namespace})
		if err = d.call(ctx, http.MethodPost, path, json.RawMessage(b), nil); err != nil {
			return fmt.Errorf("pinecone put %s: %w", name, err)
		}
	}
	return nil
}
func (d *Driver) dimension() int { d.mu.RLock(); defer d.mu.RUnlock(); return d.dims }

type pineVector struct {
	ID       string         `json:"id"`
	Values   []float32      `json:"values"`
	Metadata map[string]any `json:"metadata"`
	Score    float64        `json:"score"`
}
type fetchResponse struct {
	Vectors map[string]pineVector `json:"vectors"`
}

func (d *Driver) fetch(ctx context.Context, c column, ids []string) ([]store.Record, error) {
	var out []store.Record
	for from := 0; from < len(ids); from += 1000 {
		end := min(from+1000, len(ids))
		var res fetchResponse
		q := url.Values{}
		q.Set("namespace", c.namespace)
		for _, id := range ids[from:end] {
			q.Add("ids", id)
		}
		if err := d.call(ctx, http.MethodGet, "/vectors/fetch?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		for id, v := range res.Vectors {
			r, e := record(id, v.Metadata, v.Values)
			if e != nil {
				return nil, e
			}
			out = append(out, r)
		}
	}
	return out, nil
}
func (d *Driver) Get(ctx context.Context, name string, ids []string) ([]store.Record, error) {
	c, e := d.col(name)
	if e != nil {
		return nil, e
	}
	return d.fetch(ctx, c, ids)
}

type listResponse struct {
	Vectors []struct {
		ID string `json:"id"`
	} `json:"vectors"`
	Pagination struct {
		Next string `json:"next"`
	} `json:"pagination"`
	PaginationToken string `json:"pagination_token"`
}

func (d *Driver) listPage(ctx context.Context, c column, token string) ([]string, string, error) {
	q := url.Values{}
	q.Set("namespace", c.namespace)
	q.Set("limit", "1000")
	if token != "" {
		q.Set("paginationToken", token)
	}
	var res listResponse
	if err := d.call(ctx, http.MethodGet, "/vectors/list?"+q.Encode(), nil, &res); err != nil {
		return nil, "", fmt.Errorf("list Pinecone namespace %s (Pinecone serverless indexes support listing): %w", c.namespace, err)
	}
	ids := make([]string, len(res.Vectors))
	for i, v := range res.Vectors {
		ids[i] = v.ID
	}
	next := res.Pagination.Next
	if next == "" {
		next = res.PaginationToken
	}
	return ids, next, nil
}
func (d *Driver) list(ctx context.Context, c column) ([]string, error) {
	var ids []string
	token := ""
	for {
		page, next, err := d.listPage(ctx, c, token)
		if err != nil {
			return nil, err
		}
		ids = append(ids, page...)
		token = next
		if token == "" {
			break
		}
	}
	return ids, nil
}
func (d *Driver) Find(ctx context.Context, name string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	c, e := d.col(name)
	if e != nil {
		return nil, "", e
	}
	if limit <= 0 {
		limit = 1000
	}
	var all []store.Record
	if len(f.IDs) > 0 {
		all, e = d.fetch(ctx, c, f.IDs)
	} else {
		token := ""
		for {
			ids, next, err := d.listPage(ctx, c, token)
			if err != nil {
				return nil, "", err
			}
			page, err := d.fetch(ctx, c, ids)
			if err != nil {
				return nil, "", err
			}
			for _, r := range page {
				if f.Match(r) && r.ID > after {
					all = append(all, r)
				}
			}
			token = next
			if len(all) > limit || token == "" {
				break
			}
		}
	}
	if e != nil {
		return nil, "", e
	}
	filtered := all[:0]
	for _, r := range all {
		if f.Match(r) && r.ID > after {
			filtered = append(filtered, r)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })
	if len(filtered) <= limit {
		return filtered, "", nil
	}
	return filtered[:limit], filtered[limit-1].ID, nil
}
func (d *Driver) Delete(ctx context.Context, name string, ids []string) error {
	c, e := d.col(name)
	if e != nil {
		return e
	}
	for i := 0; i < len(ids); i += 1000 {
		end := min(i+1000, len(ids))
		if err := d.call(ctx, http.MethodPost, "/vectors/delete", map[string]any{"ids": ids[i:end], "namespace": c.namespace}, nil); err != nil {
			return fmt.Errorf("pinecone delete %s: %w", name, err)
		}
	}
	return nil
}
func (d *Driver) DeleteWhere(ctx context.Context, name string, f store.Filter) error {
	c, e := d.col(name)
	if e != nil {
		return e
	}
	ids, e := d.list(ctx, c)
	if e != nil {
		return e
	}
	recs, e := d.fetch(ctx, c, ids)
	if e != nil {
		return e
	}
	var del []string
	for _, r := range recs {
		if f.Match(r) {
			del = append(del, r.ID)
		}
	}
	return d.Delete(ctx, name, del)
}
func (d *Driver) SearchText(context.Context, string, string, store.Filter, int) ([]store.Hit, error) {
	return nil, store.ErrUnsupported
}
func (d *Driver) SearchVector(ctx context.Context, name, model string, v []float32, f store.Filter, k int) ([]store.Hit, error) {
	c, e := d.col(name)
	if e != nil {
		return nil, e
	}
	if !c.vector {
		return nil, store.ErrUnsupported
	}
	if len(v) != d.dimension() {
		return nil, fmt.Errorf("pinecone: query has %d dimensions, index has %d", len(v), d.dimension())
	}
	if k < 1 {
		k = 10
	}
	filters := map[string]any{"sj_has_vector": map[string]any{"$eq": true}}
	if len(f.IDs) > 0 {
		filters["sj_id"] = map[string]any{"$in": f.IDs}
	}
	if model != "" {
		filters["sj_vector_model"] = map[string]any{"$eq": model}
	}
	if f.Doc != "" {
		filters["sj_doc"] = map[string]any{"$eq": f.Doc}
	}
	if f.Parent != "" {
		filters["sj_parent"] = map[string]any{"$eq": f.Parent}
	}
	if f.Kind != "" {
		filters["sj_kind"] = map[string]any{"$eq": f.Kind}
	}
	if f.Type != "" {
		filters["sj_type"] = map[string]any{"$eq": f.Type}
	}
	body := map[string]any{"vector": v, "topK": k, "namespace": c.namespace, "filter": filters, "includeMetadata": true, "includeValues": true}
	var res struct {
		Matches []pineVector `json:"matches"`
	}
	if e = d.call(ctx, http.MethodPost, "/query", body, &res); e != nil {
		return nil, e
	}
	hits := make([]store.Hit, 0, len(res.Matches))
	for _, v := range res.Matches {
		r, e := record(v.ID, v.Metadata, v.Values)
		if e != nil {
			return nil, e
		}
		if f.Match(r) {
			hits = append(hits, store.Hit{Record: r, Score: v.Score})
		}
	}
	return hits, nil
}
func record(id string, m map[string]any, values []float32) (store.Record, error) {
	str := func(k string) string { s, _ := m[k].(string); return s }
	num := func(k string) uint64 {
		switch v := m[k].(type) {
		case float64:
			return uint64(v)
		case json.Number:
			n, _ := strconv.ParseUint(string(v), 10, 64)
			return n
		}
		return 0
	}
	r := store.Record{ID: id, Doc: str("sj_doc"), Parent: str("sj_parent"), Kind: str("sj_kind"), Type: str("sj_type"), Order: str("sj_order"), Rev: num("sj_rev"), TextRev: num("sj_text_rev"), CRev: num("sj_crev"), Seq: num("sj_seq"), Layout: str("sj_layout"), Text: str("sj_text"), VectorModel: str("sj_vector_model"), VectorMeta: json.RawMessage(str("sj_vector_meta")), Node: json.RawMessage(str("sj_node"))}
	if s := str("sj_updated"); s != "" {
		r.Updated, _ = time.Parse(time.RFC3339Nano, s)
	}
	if m["sj_has_vector"] == true {
		r.Vector = values
	}
	return r, nil
}
