// Package chroma stores records in Chroma collections (R-DRV-05), over its v2
// REST API. A record's Chroma id is its ID, its Chroma document is its search
// text, and the envelope and ScreenJSON go in flat metadata (sj_doc, sj_kind,
// …, node_json). Chroma 1.5 requires an embedding on every record, so a
// record without a native vector gets a zero vector of the collection's size
// with sj_vec=false, and vector searches filter on sj_vec=true.
package chroma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Driver is a Chroma server.
type Driver struct {
	base string // …/api/v2/tenants/{t}/databases/{d}
	root string
	http *http.Client
	mu   sync.RWMutex
	ids  map[string]string // collection name to Chroma id
	dims map[string]int    // collection name to vector size
	vec  map[string]bool   // collections with a declared native vector
}

// Open connects to Chroma at rawURL, using tenant and database (created if
// missing).
func Open(ctx context.Context, rawURL, tenant, database string) (*Driver, error) {
	if tenant == "" {
		tenant = "default_tenant"
	}
	if database == "" {
		database = "default_database"
	}
	root := strings.TrimRight(rawURL, "/")
	d := &Driver{
		root: root,
		base: root + "/api/v2/tenants/" + url.PathEscape(tenant) + "/databases/" + url.PathEscape(database),
		http: &http.Client{Timeout: 60 * time.Second},
		ids:  map[string]string{}, dims: map[string]int{}, vec: map[string]bool{},
	}
	if err := d.Ping(ctx); err != nil {
		return nil, err
	}
	// Tenant and database may already exist; errors here are only fatal if the
	// database is then unusable, which Ensure finds out.
	_, _ = d.call(ctx, http.MethodPost, root+"/api/v2/tenants", map[string]any{"name": tenant}, nil)
	_, _ = d.call(ctx, http.MethodPost, root+"/api/v2/tenants/"+url.PathEscape(tenant)+"/databases", map[string]any{"name": database}, nil)
	return d, nil
}

// Name implements store.Driver.
func (d *Driver) Name() string { return "chroma" }

// Caps implements store.Driver.
func (d *Driver) Caps() store.Caps {
	return store.Caps{FullText: true, Vector: true}
}

func (d *Driver) call(ctx context.Context, method, u string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("chroma: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 300 {
		if len(raw) > 500 {
			raw = raw[:500]
		}
		return res.StatusCode, fmt.Errorf("chroma: %s %s: %s: %s", method, strings.TrimPrefix(u, d.root), res.Status, raw)
	}
	if out != nil && len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(out); err != nil {
			return res.StatusCode, fmt.Errorf("chroma: decode: %w", err)
		}
	}
	return res.StatusCode, nil
}

type collection struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata"`
}

// Ensure implements store.Driver.
func (d *Driver) Ensure(ctx context.Context, cols []store.CollectionSpec) error {
	for _, c := range cols {
		dims := 1
		if c.Vector != nil {
			dims = c.Vector.Dimensions
		}
		var got collection
		body := map[string]any{"name": c.Name, "get_or_create": true, "metadata": map[string]any{"sj_dims": dims}}
		if _, err := d.call(ctx, http.MethodPost, d.base+"/collections", body, &got); err != nil {
			return fmt.Errorf("chroma: ensure %s: %w", c.Name, err)
		}
		if n, ok := got.Metadata["sj_dims"].(json.Number); ok {
			if stored, _ := n.Int64(); int(stored) != dims {
				return fmt.Errorf("chroma: collection %s holds %d-dimensional vectors, not %d", c.Name, stored, dims)
			}
		}
		d.mu.Lock()
		d.ids[c.Name], d.dims[c.Name], d.vec[c.Name] = got.ID, dims, c.Vector != nil
		d.mu.Unlock()
	}
	return nil
}

// coll finds a collection's endpoint.
func (d *Driver) coll(ctx context.Context, name string) (string, int, error) {
	d.mu.RLock()
	id, ok := d.ids[name]
	dims := d.dims[name]
	d.mu.RUnlock()
	if ok {
		return d.base + "/collections/" + id, dims, nil
	}
	var got collection
	if _, err := d.call(ctx, http.MethodGet, d.base+"/collections/"+url.PathEscape(name), nil, &got); err != nil {
		return "", 0, fmt.Errorf("chroma: no collection %s: %w", name, err)
	}
	dims = 1
	if n, ok := got.Metadata["sj_dims"].(json.Number); ok {
		v, _ := n.Int64()
		dims = int(v)
	}
	d.mu.Lock()
	d.ids[name], d.dims[name] = got.ID, dims
	d.mu.Unlock()
	return d.base + "/collections/" + got.ID, dims, nil
}

// Tx implements store.Driver; Chroma has no transactions.
func (d *Driver) Tx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

func metadata(r store.Record) map[string]any {
	m := map[string]any{
		"sj_id": r.ID, "sj_doc": r.Doc, "sj_parent": r.Parent, "sj_kind": r.Kind, "sj_type": r.Type,
		"sj_order": r.Order, "sj_rev": int64(r.Rev), "sj_text_rev": int64(r.TextRev), "sj_crev": int64(r.CRev),
		"sj_seq": int64(r.Seq), "sj_layout": r.Layout, "sj_vec": r.Vector != nil, "sj_vector_model": r.VectorModel,
		"sj_vector_meta": string(r.VectorMeta), "node_json": string(r.Node), "sj_updated": "",
	}
	if !r.Updated.IsZero() {
		m["sj_updated"] = r.Updated.UTC().Format(time.RFC3339Nano)
	}
	return m
}

// Put implements store.Driver.
func (d *Driver) Put(ctx context.Context, col string, recs []store.Record) error {
	if len(recs) == 0 {
		return nil
	}
	u, dims, err := d.coll(ctx, col)
	if err != nil {
		return err
	}
	for start := 0; start < len(recs); start += 500 {
		batch := recs[start:min(start+500, len(recs))]
		ids := make([]string, len(batch))
		docs := make([]string, len(batch))
		metas := make([]map[string]any, len(batch))
		embs := make([][]float32, len(batch))
		for i, r := range batch {
			ids[i], docs[i], metas[i] = r.ID, r.Text, metadata(r)
			if r.Vector != nil {
				if len(r.Vector) != dims {
					return fmt.Errorf("chroma: %s has a %d-dimensional vector; %s holds %d", r.ID, len(r.Vector), col, dims)
				}
				embs[i] = r.Vector
			} else {
				embs[i] = make([]float32, dims)
			}
		}
		body := map[string]any{"ids": ids, "documents": docs, "metadatas": metas, "embeddings": embs}
		if _, err := d.call(ctx, http.MethodPost, u+"/upsert", body, nil); err != nil {
			return fmt.Errorf("chroma: put into %s: %w", col, err)
		}
	}
	return nil
}

// getResponse is what /get returns.
type getResponse struct {
	IDs        []string         `json:"ids"`
	Documents  []*string        `json:"documents"`
	Metadatas  []map[string]any `json:"metadatas"`
	Embeddings [][]float32      `json:"embeddings"`
}

func record(id string, doc *string, meta map[string]any, emb []float32) (store.Record, error) {
	r := store.Record{ID: id}
	str := func(k string) string {
		s, _ := meta[k].(string)
		return s
	}
	num := func(k string) uint64 {
		if n, ok := meta[k].(json.Number); ok {
			v, _ := n.Int64()
			return uint64(v)
		}
		return 0
	}
	r.Doc, r.Parent, r.Kind, r.Type, r.Order, r.Layout = str("sj_doc"), str("sj_parent"), str("sj_kind"), str("sj_type"), str("sj_order"), str("sj_layout")
	r.Rev, r.TextRev, r.CRev, r.Seq = num("sj_rev"), num("sj_text_rev"), num("sj_crev"), num("sj_seq")
	if s := str("sj_updated"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return r, err
		}
		r.Updated = t.UTC()
	}
	if s := str("node_json"); s != "" {
		r.Node = json.RawMessage(s)
	}
	if s := str("sj_vector_meta"); s != "" {
		r.VectorMeta = json.RawMessage(s)
	}
	if doc != nil {
		r.Text = *doc
	}
	if v, _ := meta["sj_vec"].(bool); v {
		r.Vector, r.VectorModel = emb, str("sj_vector_model")
	}
	return r, nil
}

func (g getResponse) records() ([]store.Record, error) {
	out := make([]store.Record, 0, len(g.IDs))
	for i, id := range g.IDs {
		var doc *string
		if i < len(g.Documents) {
			doc = g.Documents[i]
		}
		var emb []float32
		if i < len(g.Embeddings) {
			emb = g.Embeddings[i]
		}
		r, err := record(id, doc, g.Metadatas[i], emb)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

var include = []string{"documents", "metadatas", "embeddings"}

// Get implements store.Driver.
func (d *Driver) Get(ctx context.Context, col string, ids []string) ([]store.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	u, _, err := d.coll(ctx, col)
	if err != nil {
		return nil, err
	}
	var res getResponse
	if _, err := d.call(ctx, http.MethodPost, u+"/get", map[string]any{"ids": ids, "include": include}, &res); err != nil {
		return nil, fmt.Errorf("chroma: get from %s: %w", col, err)
	}
	return res.records()
}

// where turns a filter into a Chroma where clause. An empty filter matches
// every record through sj_id, since Chroma wants a where or ids.
func where(f store.Filter, extra ...map[string]any) map[string]any {
	var conds []map[string]any
	eq := func(k, v string) {
		if v != "" {
			conds = append(conds, map[string]any{k: map[string]any{"$eq": v}})
		}
	}
	eq("sj_doc", f.Doc)
	eq("sj_parent", f.Parent)
	eq("sj_kind", f.Kind)
	eq("sj_type", f.Type)
	if len(f.IDs) > 0 {
		conds = append(conds, map[string]any{"sj_id": map[string]any{"$in": f.IDs}})
	}
	conds = append(conds, extra...)
	switch len(conds) {
	case 0:
		return map[string]any{"sj_id": map[string]any{"$ne": ""}}
	case 1:
		return conds[0]
	}
	list := make([]any, len(conds))
	for i, c := range conds {
		list[i] = c
	}
	return map[string]any{"$and": list}
}

// Find implements store.Driver. Chroma pages by offset, so the cursor is the
// offset of the next page; pages are consistent while the collection isn't
// changing under them.
func (d *Driver) Find(ctx context.Context, col string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("chroma: limit must be positive")
	}
	u, _, err := d.coll(ctx, col)
	if err != nil {
		return nil, "", err
	}
	offset := 0
	if after != "" {
		if offset, err = strconv.Atoi(after); err != nil {
			return nil, "", fmt.Errorf("chroma: bad cursor %q", after)
		}
	}
	var res getResponse
	body := map[string]any{"where": where(f), "limit": limit + 1, "offset": offset, "include": include}
	if _, err := d.call(ctx, http.MethodPost, u+"/get", body, &res); err != nil {
		return nil, "", fmt.Errorf("chroma: find in %s: %w", col, err)
	}
	recs, err := res.records()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		next = strconv.Itoa(offset + limit)
	}
	return recs, next, nil
}

// Delete implements store.Driver.
func (d *Driver) Delete(ctx context.Context, col string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	u, _, err := d.coll(ctx, col)
	if err != nil {
		return err
	}
	if _, err := d.call(ctx, http.MethodPost, u+"/delete", map[string]any{"ids": ids}, nil); err != nil {
		return fmt.Errorf("chroma: delete from %s: %w", col, err)
	}
	return nil
}

// DeleteWhere implements store.Driver.
func (d *Driver) DeleteWhere(ctx context.Context, col string, f store.Filter) error {
	u, _, err := d.coll(ctx, col)
	if err != nil {
		return err
	}
	if _, err := d.call(ctx, http.MethodPost, u+"/delete", map[string]any{"where": where(f)}, nil); err != nil {
		return fmt.Errorf("chroma: delete from %s: %w", col, err)
	}
	return nil
}

// SearchText implements store.Driver with a where_document $contains, which
// is what Chroma offers (R-SRCH-02). Every hit scores one.
func (d *Driver) SearchText(ctx context.Context, col, q string, f store.Filter, k int) ([]store.Hit, error) {
	u, _, err := d.coll(ctx, col)
	if err != nil {
		return nil, err
	}
	var res getResponse
	body := map[string]any{"where": where(f), "where_document": map[string]any{"$contains": q}, "limit": k, "include": include}
	if _, err := d.call(ctx, http.MethodPost, u+"/get", body, &res); err != nil {
		return nil, fmt.Errorf("chroma: text search in %s: %w", col, err)
	}
	recs, err := res.records()
	if err != nil {
		return nil, err
	}
	out := make([]store.Hit, len(recs))
	for i, r := range recs {
		out[i] = store.Hit{Record: r, Score: 1}
	}
	return out, nil
}

// SearchVector implements store.Driver with a nearest-neighbour query over
// records that hold a real vector of the model.
func (d *Driver) SearchVector(ctx context.Context, col, model string, v []float32, f store.Filter, k int) ([]store.Hit, error) {
	u, dims, err := d.coll(ctx, col)
	if err != nil {
		return nil, err
	}
	d.mu.RLock()
	declared := d.vec[col]
	d.mu.RUnlock()
	if !declared || len(v) != dims {
		return nil, store.ErrUnsupported
	}
	var res struct {
		IDs        [][]string         `json:"ids"`
		Documents  [][]*string        `json:"documents"`
		Metadatas  [][]map[string]any `json:"metadatas"`
		Embeddings [][][]float32      `json:"embeddings"`
		Distances  [][]float64        `json:"distances"`
	}
	body := map[string]any{
		"query_embeddings": [][]float32{v}, "n_results": k,
		"where":   where(f, map[string]any{"sj_vec": map[string]any{"$eq": true}}, map[string]any{"sj_vector_model": map[string]any{"$eq": model}}),
		"include": []string{"documents", "metadatas", "embeddings", "distances"},
	}
	if _, err := d.call(ctx, http.MethodPost, u+"/query", body, &res); err != nil {
		return nil, fmt.Errorf("chroma: vector search in %s: %w", col, err)
	}
	if len(res.IDs) == 0 {
		return nil, nil
	}
	out := make([]store.Hit, 0, len(res.IDs[0]))
	for i, id := range res.IDs[0] {
		r, err := record(id, res.Documents[0][i], res.Metadatas[0][i], res.Embeddings[0][i])
		if err != nil {
			return nil, err
		}
		out = append(out, store.Hit{Record: r, Score: 1 / (1 + res.Distances[0][i])})
	}
	return out, nil
}

// Ping implements store.Driver.
func (d *Driver) Ping(ctx context.Context) error {
	_, err := d.call(ctx, http.MethodGet, d.root+"/api/v2/heartbeat", nil, nil)
	return err
}

// Close implements store.Driver.
func (d *Driver) Close() error {
	d.http.CloseIdleConnections()
	return nil
}
