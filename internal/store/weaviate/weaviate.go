// Package weaviate stores records as Weaviate objects (R-DRV-06): one class
// per collection (PascalCase, prefixed), the object UUID is the record ID, the
// envelope is flat properties, nodeJson holds the ScreenJSON unindexed, and
// text is BM25-searchable. A level with a native vector gets a named vector,
// "native", with the vectoriser off.
//
// Filtered Get queries stop at 10,000 results, so Find pages by key rather
// than offset: each page asks for keys greater than the last one seen, which
// has no cap however large a document is.
package weaviate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Driver is a Weaviate server.
type Driver struct {
	base   string
	prefix string
	http   *http.Client
	mu     sync.RWMutex
	vec    map[string]bool // class has the native vector
}

// Open connects to Weaviate at rawURL; prefix starts every class name.
func Open(ctx context.Context, rawURL, prefix string) (*Driver, error) {
	if prefix == "" {
		prefix = "Sj"
	}
	d := &Driver{base: strings.TrimRight(rawURL, "/"), prefix: prefix, http: &http.Client{Timeout: 60 * time.Second}, vec: map[string]bool{}}
	if err := d.Ping(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Name implements store.Driver.
func (d *Driver) Name() string { return "weaviate" }

// Caps implements store.Driver.
func (d *Driver) Caps() store.Caps { return store.Caps{FullText: true, Vector: true} }

// Class is a collection's class name: the prefix and the collection name in
// PascalCase ("elements" becomes "SjElements").
func (d *Driver) Class(col string) string {
	var b strings.Builder
	b.WriteString(d.prefix)
	up := true
	for _, r := range col {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
			up = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (d *Driver) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
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
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("weaviate: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotFound {
		if len(raw) > 600 {
			raw = raw[:600]
		}
		return res.StatusCode, fmt.Errorf("weaviate: %s %s: %s: %s", method, path, res.Status, raw)
	}
	if out != nil && len(raw) > 0 && res.StatusCode != http.StatusNotFound {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(out); err != nil {
			return res.StatusCode, fmt.Errorf("weaviate: decode: %w", err)
		}
	}
	return res.StatusCode, nil
}

// Ensure implements store.Driver.
func (d *Driver) Ensure(ctx context.Context, cols []store.CollectionSpec) error {
	for _, c := range cols {
		class := d.Class(c.Name)
		d.mu.Lock()
		d.vec[class] = c.Vector != nil
		d.mu.Unlock()
		status, err := d.call(ctx, http.MethodGet, "/v1/schema/"+class, nil, nil)
		if err != nil {
			return err
		}
		if status == http.StatusOK {
			continue
		}
		field := func(name string) map[string]any {
			return map[string]any{"name": name, "dataType": []string{"text"}, "tokenization": "field", "indexSearchable": false}
		}
		num := func(name string) map[string]any { return map[string]any{"name": name, "dataType": []string{"int"}} }
		stored := func(name string) map[string]any {
			return map[string]any{"name": name, "dataType": []string{"text"}, "indexFilterable": false, "indexSearchable": false}
		}
		props := []any{
			field("sjKey"), field("sjDoc"), field("sjParent"), field("sjKind"), field("sjType"), field("sjOrder"),
			num("sjRev"), num("sjTextRev"), num("sjCrev"), num("sjSeq"),
			map[string]any{"name": "sjUpdated", "dataType": []string{"date"}},
			field("sjLayout"), field("sjVectorModel"), stored("sjVectorMeta"),
			map[string]any{"name": "sjVec", "dataType": []string{"boolean"}},
			stored("nodeJson"),
			map[string]any{"name": "text", "dataType": []string{"text"}, "tokenization": "word", "indexSearchable": true},
		}
		body := map[string]any{"class": class, "properties": props}
		if c.Vector != nil {
			body["vectorConfig"] = map[string]any{"native": map[string]any{
				"vectorizer": map[string]any{"none": map[string]any{}}, "vectorIndexType": "hnsw",
				"vectorIndexConfig": map[string]any{"distance": "cosine"},
			}}
		} else {
			body["vectorizer"] = "none"
		}
		if _, err := d.call(ctx, http.MethodPost, "/v1/schema", body, nil); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("weaviate: ensure %s: %w", class, err)
		}
	}
	return nil
}

func (d *Driver) hasVector(class string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.vec[class]
}

// Tx implements store.Driver; Weaviate has no transactions.
func (d *Driver) Tx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// Put implements store.Driver with the batch endpoint, which replaces objects
// with the same UUID.
func (d *Driver) Put(ctx context.Context, col string, recs []store.Record) error {
	class := d.Class(col)
	for start := 0; start < len(recs); start += 200 {
		batch := recs[start:min(start+200, len(recs))]
		objs := make([]any, len(batch))
		for i, r := range batch {
			props := map[string]any{
				"sjKey": r.ID, "sjDoc": r.Doc, "sjParent": r.Parent, "sjKind": r.Kind, "sjType": r.Type, "sjOrder": r.Order,
				"sjRev": r.Rev, "sjTextRev": r.TextRev, "sjCrev": r.CRev, "sjSeq": r.Seq, "sjLayout": r.Layout,
				"sjVectorModel": r.VectorModel, "sjVectorMeta": string(r.VectorMeta), "sjVec": r.Vector != nil,
				"nodeJson": string(r.Node), "text": r.Text,
			}
			if !r.Updated.IsZero() {
				props["sjUpdated"] = r.Updated.UTC().Format(time.RFC3339Nano)
			}
			obj := map[string]any{"class": class, "id": r.ID, "properties": props}
			if r.Vector != nil {
				if !d.hasVector(class) {
					return fmt.Errorf("weaviate: %s declares no vector, but %s has one", col, r.ID)
				}
				obj["vectors"] = map[string]any{"native": r.Vector}
			}
			objs[i] = obj
		}
		var res []struct {
			ID     string `json:"id"`
			Result struct {
				Errors *struct {
					Error []struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"errors"`
			} `json:"result"`
		}
		if _, err := d.call(ctx, http.MethodPost, "/v1/batch/objects?consistency_level=ALL", map[string]any{"objects": objs}, &res); err != nil {
			return fmt.Errorf("weaviate: put into %s: %w", col, err)
		}
		for _, o := range res {
			if o.Result.Errors != nil && len(o.Result.Errors.Error) > 0 {
				return fmt.Errorf("weaviate: put %s: %s", o.ID, o.Result.Errors.Error[0].Message)
			}
		}
	}
	return nil
}

// gql is a GraphQL value writer: enums unquoted, everything else as JSON.
type enum string

func gql(v any) string {
	switch x := v.(type) {
	case enum:
		return string(x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ":" + gql(x[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = gql(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case []float32:
		parts := make([]string, len(x))
		for i, f := range x {
			parts[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func eq(path, v string) map[string]any {
	return map[string]any{"path": []any{path}, "operator": enum("Equal"), "valueText": v}
}

// where builds a where filter, or nil for none.
func where(f store.Filter, extra ...map[string]any) map[string]any {
	var ops []any
	if f.Doc != "" {
		ops = append(ops, eq("sjDoc", f.Doc))
	}
	if f.Parent != "" {
		ops = append(ops, eq("sjParent", f.Parent))
	}
	if f.Kind != "" {
		ops = append(ops, eq("sjKind", f.Kind))
	}
	if f.Type != "" {
		ops = append(ops, eq("sjType", f.Type))
	}
	if len(f.IDs) > 0 {
		vals := make([]any, len(f.IDs))
		for i, id := range f.IDs {
			vals[i] = id
		}
		ops = append(ops, map[string]any{"path": []any{"sjKey"}, "operator": enum("ContainsAny"), "valueText": vals})
	}
	for _, e := range extra {
		ops = append(ops, e)
	}
	switch len(ops) {
	case 0:
		return nil
	case 1:
		return ops[0].(map[string]any)
	}
	return map[string]any{"operator": enum("And"), "operands": ops}
}

const fields = "sjKey sjDoc sjParent sjKind sjType sjOrder sjRev sjTextRev sjCrev sjSeq sjUpdated sjLayout sjVectorModel sjVectorMeta sjVec nodeJson text"

// get runs a GraphQL Get and returns the objects.
func (d *Driver) get(ctx context.Context, class, args, additional string) ([]map[string]any, error) {
	q := fmt.Sprintf("{Get{%s%s{%s _additional{id %s}}}}", class, args, fields, additional)
	var res struct {
		Data struct {
			Get map[string][]map[string]any `json:"Get"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := d.call(ctx, http.MethodPost, "/v1/graphql", map[string]any{"query": q}, &res); err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("weaviate: %s", res.Errors[0].Message)
	}
	return res.Data.Get[class], nil
}

func (d *Driver) args(parts map[string]any) string {
	if len(parts) == 0 {
		return ""
	}
	s := gql(parts)
	return "(" + s[1:len(s)-1] + ")"
}

func (d *Driver) vectorField(class string) string {
	if d.hasVector(class) {
		return "vectors{native}"
	}
	return ""
}

func record(o map[string]any) (store.Record, error) {
	str := func(k string) string {
		s, _ := o[k].(string)
		return s
	}
	num := func(k string) uint64 {
		if n, ok := o[k].(json.Number); ok {
			v, _ := n.Int64()
			return uint64(v)
		}
		return 0
	}
	r := store.Record{
		ID: str("sjKey"), Doc: str("sjDoc"), Parent: str("sjParent"), Kind: str("sjKind"), Type: str("sjType"),
		Order: str("sjOrder"), Rev: num("sjRev"), TextRev: num("sjTextRev"), CRev: num("sjCrev"), Seq: num("sjSeq"),
		Layout: str("sjLayout"), Text: str("text"),
	}
	if s := str("sjUpdated"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return r, err
		}
		r.Updated = t.UTC()
	}
	if s := str("nodeJson"); s != "" {
		r.Node = json.RawMessage(s)
	}
	if s := str("sjVectorMeta"); s != "" {
		r.VectorMeta = json.RawMessage(s)
	}
	if v, _ := o["sjVec"].(bool); v {
		r.VectorModel = str("sjVectorModel")
		add, _ := o["_additional"].(map[string]any)
		vecs, _ := add["vectors"].(map[string]any)
		if list, ok := vecs["native"].([]any); ok {
			r.Vector = make([]float32, len(list))
			for i, x := range list {
				if n, ok := x.(json.Number); ok {
					f, _ := n.Float64()
					r.Vector[i] = float32(f)
				}
			}
		}
	}
	return r, nil
}

func records(objs []map[string]any) ([]store.Record, error) {
	out := make([]store.Record, 0, len(objs))
	for _, o := range objs {
		r, err := record(o)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Get implements store.Driver.
func (d *Driver) Get(ctx context.Context, col string, ids []string) ([]store.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	class := d.Class(col)
	var out []store.Record
	for start := 0; start < len(ids); start += 1000 {
		part := ids[start:min(start+1000, len(ids))]
		objs, err := d.get(ctx, class, d.args(map[string]any{"where": where(store.Filter{IDs: part}), "limit": len(part)}), d.vectorField(class))
		if err != nil {
			return nil, fmt.Errorf("weaviate: get from %s: %w", col, err)
		}
		recs, err := records(objs)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

// Find implements store.Driver, paging by key: sjKey greater than the last
// key seen, sorted by sjKey.
func (d *Driver) Find(ctx context.Context, col string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("weaviate: limit must be positive")
	}
	class := d.Class(col)
	var extra []map[string]any
	if after != "" {
		extra = append(extra, map[string]any{"path": []any{"sjKey"}, "operator": enum("GreaterThan"), "valueText": after})
	}
	args := map[string]any{"limit": limit + 1, "sort": []any{map[string]any{"path": []any{"sjKey"}, "order": enum("asc")}}}
	if w := where(f, extra...); w != nil {
		args["where"] = w
	}
	objs, err := d.get(ctx, class, d.args(args), d.vectorField(class))
	if err != nil {
		return nil, "", fmt.Errorf("weaviate: find in %s: %w", col, err)
	}
	recs, err := records(objs)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		next = recs[limit-1].ID
	}
	return recs, next, nil
}

// deleteWhere removes the objects matching a where filter.
func (d *Driver) deleteWhere(ctx context.Context, class string, w map[string]any) error {
	if w == nil {
		w = map[string]any{"path": []any{"sjKey"}, "operator": "Like", "valueText": "*"}
	} else {
		w = toJSONWhere(w)
	}
	body := map[string]any{"match": map[string]any{"class": class, "where": w}, "output": "minimal"}
	_, err := d.call(ctx, http.MethodDelete, "/v1/batch/objects?consistency_level=ALL", body, nil)
	return err
}

// toJSONWhere turns a GraphQL-style where (with enum operators) into the REST
// form, where operators are plain strings.
func toJSONWhere(w map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range w {
		switch x := v.(type) {
		case enum:
			out[k] = string(x)
		case []any:
			if k == "valueText" {
				// REST takes a list of texts as valueTextArray.
				out["valueTextArray"] = x
				continue
			}
			list := make([]any, len(x))
			for i, e := range x {
				if m, ok := e.(map[string]any); ok {
					list[i] = toJSONWhere(m)
				} else {
					list[i] = e
				}
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}

// Delete implements store.Driver.
func (d *Driver) Delete(ctx context.Context, col string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := d.deleteWhere(ctx, d.Class(col), where(store.Filter{IDs: ids})); err != nil {
		return fmt.Errorf("weaviate: delete from %s: %w", col, err)
	}
	return nil
}

// DeleteWhere implements store.Driver.
func (d *Driver) DeleteWhere(ctx context.Context, col string, f store.Filter) error {
	if err := d.deleteWhere(ctx, d.Class(col), where(f)); err != nil {
		return fmt.Errorf("weaviate: delete from %s: %w", col, err)
	}
	return nil
}

func scored(objs []map[string]any, score func(add map[string]any) float64) ([]store.Hit, error) {
	out := make([]store.Hit, 0, len(objs))
	for _, o := range objs {
		r, err := record(o)
		if err != nil {
			return nil, err
		}
		add, _ := o["_additional"].(map[string]any)
		out = append(out, store.Hit{Record: r, Score: score(add)})
	}
	return out, nil
}

func number(v any) float64 {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// SearchText implements store.Driver with BM25 on text.
func (d *Driver) SearchText(ctx context.Context, col, q string, f store.Filter, k int) ([]store.Hit, error) {
	class := d.Class(col)
	args := map[string]any{"bm25": map[string]any{"query": q, "properties": []any{"text"}}, "limit": k}
	if w := where(f); w != nil {
		args["where"] = w
	}
	objs, err := d.get(ctx, class, d.args(args), "score "+d.vectorField(class))
	if err != nil {
		return nil, fmt.Errorf("weaviate: text search in %s: %w", col, err)
	}
	return scored(objs, func(add map[string]any) float64 { return number(add["score"]) })
}

// SearchVector implements store.Driver with nearVector on the native vector.
func (d *Driver) SearchVector(ctx context.Context, col, model string, v []float32, f store.Filter, k int) ([]store.Hit, error) {
	class := d.Class(col)
	if !d.hasVector(class) {
		return nil, store.ErrUnsupported
	}
	args := map[string]any{
		"nearVector": map[string]any{"vector": v, "targetVectors": []any{"native"}},
		"limit":      k,
		"where": where(f, map[string]any{"path": []any{"sjVec"}, "operator": enum("Equal"), "valueBoolean": true},
			eq("sjVectorModel", model)),
	}
	objs, err := d.get(ctx, class, d.args(args), "distance vectors{native}")
	if err != nil {
		return nil, fmt.Errorf("weaviate: vector search in %s: %w", col, err)
	}
	return scored(objs, func(add map[string]any) float64 { return 1 - number(add["distance"]) })
}

// Ping implements store.Driver.
func (d *Driver) Ping(ctx context.Context) error {
	status, err := d.call(ctx, http.MethodGet, "/v1/.well-known/ready", nil, nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("weaviate: not ready (%d)", status)
	}
	return err
}

// Close implements store.Driver.
func (d *Driver) Close() error {
	d.http.CloseIdleConnections()
	return nil
}
