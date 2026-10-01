package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Do runs a request on a document route. The API layer handles the routes
// outside documents (tokens, passes, search, system) and the action routes
// served by other packages (commits, export, exports).
func (m *Manager) Do(ctx context.Context, req *Request, method string) (*Response, error) {
	tg := req.Target
	r := tg.Route
	if tg.Wild {
		if tg.Suffix == paths.SuffixCheckout {
			return m.BulkCheckout(ctx, req)
		}
		return m.WildcardList(ctx, req)
	}
	switch tg.Suffix {
	case paths.SuffixLang:
		return m.Lang(ctx, req, method)
	case paths.SuffixSet:
		return m.Set(ctx, req, method)
	case paths.SuffixEmbeddings, paths.SuffixEmbedding:
		return m.Embeddings(ctx, req, method)
	case paths.SuffixCheckout:
		if r.List {
			return m.BulkCheckout(ctx, req)
		}
		return m.Checkout(ctx, req, method)
	case paths.SuffixMove:
		return m.Move(ctx, req)
	}
	switch r.Name {
	case "documents":
		if method == http.MethodPost {
			return m.Create(ctx, req)
		}
		return m.List(req)
	case "document":
		switch method {
		case http.MethodPut:
			return m.Replace(ctx, req)
		case http.MethodDelete:
			return m.DeleteDocument(ctx, req)
		case http.MethodPatch:
			return m.Patch(ctx, req)
		}
		return m.Get(ctx, req)
	case "node", "element.untyped":
		_, _, err := m.resolveNode(ctx, tg)
		if err == nil {
			err = notFound("no node here")
		}
		return nil, err
	case "cast":
		return m.Cast(ctx, req)
	case "appearances":
		return m.Appearances(ctx, req)
	case "checkouts":
		return m.Checkouts(ctx, tg.Doc())
	}
	if r.List {
		if method == http.MethodPost {
			return m.Insert(ctx, req)
		}
		return m.ListGet(ctx, req)
	}
	switch method {
	case http.MethodPatch:
		return m.Patch(ctx, req)
	case http.MethodPut:
		return m.Put(ctx, req)
	case http.MethodDelete:
		return m.Delete(ctx, req)
	}
	return m.Get(ctx, req)
}

// searchBody is POST /search (SPEC.md 6.12).
type searchBody struct {
	Scope  string         `json:"scope"`
	Q      string         `json:"q"`
	Vector []float32      `json:"vector"`
	Model  string         `json:"model"`
	K      int            `json:"k"`
	Where  map[string]any `json:"where"`
}

// Search runs a text or vector search through the driver.
func (m *Manager) Search(ctx context.Context, body []byte) (*Response, error) {
	var b searchBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&b); err != nil {
		return nil, badRequest("search body: %v", err)
	}
	if (b.Q == "") == (len(b.Vector) == 0) {
		return nil, invalidf("", "give exactly one of q or vector (R-SRCH-01)")
	}
	tg, err := m.o.Paths.Parse(http.MethodGet, b.Scope)
	if err != nil {
		return nil, invalidf("/scope", "scope: %v", err)
	}
	kind := tg.Route.Kind
	if !tg.Route.List || (kind != model.KindScene && kind != model.KindElement && kind != model.KindCharacter) {
		return nil, invalidf("/scope", "scope must be a list of scenes, elements or characters")
	}
	l := m.o.Layout
	if !l.Splits(kind) {
		return nil, newErr(http.StatusNotImplemented, CodeUnsupported,
			"%ss are not stored as records of their own under this layout, so they can't be searched", kind)
	}
	f := store.Filter{Kind: string(kind), Type: tg.Param(paths.ParamType)}
	if d := tg.Doc(); d != paths.Wildcard {
		f.Doc = d
	}
	if s := tg.Param(paths.ParamScene); s != "" && s != paths.Wildcard && kind == model.KindElement {
		f.Parent = s
	}
	k := b.K
	if k <= 0 {
		k = 20
	}
	if k > 1000 {
		k = 1000
	}
	fetch := k
	if len(b.Where) > 0 {
		fetch = min(k*5, 5000)
	}

	col := l.Collection(kind)
	var hits []store.Hit
	if b.Q != "" {
		hits, err = m.o.Driver.SearchText(ctx, col, b.Q, f, fetch)
	} else {
		vec := l.VectorFor(kind)
		if vec == nil || vec.Model != b.Model {
			return nil, newErr(http.StatusNotImplemented, CodeUnsupported,
				"no native vector for model %q at this level (R-SRCH-03)", b.Model)
		}
		if len(b.Vector) != vec.Dimensions {
			return nil, invalidf("/vector", "model %s has %d dimensions, not %d", vec.Model, vec.Dimensions, len(b.Vector))
		}
		hits, err = m.o.Driver.SearchVector(ctx, col, b.Model, b.Vector, f, fetch)
	}
	if errors.Is(err, store.ErrUnsupported) {
		return nil, newErr(http.StatusNotImplemented, CodeUnsupported, "the %s driver can't do this search", m.o.Driver.Name())
	}
	if err != nil {
		return nil, storageErr(err)
	}

	out := []any{}
	for _, h := range hits {
		if len(out) == k {
			break
		}
		node, err := decodeJSON(h.Record.Node)
		if err != nil {
			continue
		}
		if len(b.Where) > 0 {
			nm, _ := node.(map[string]any)
			if !matchWhere(&model.Node{Fields: nm}, b.Where) {
				continue
			}
		}
		out = append(out, map[string]any{
			"path": recordPath(h.Record), "id": h.Record.ID, "doc": h.Record.Doc,
			"score": h.Score, "node": node,
		})
	}
	return &Response{Status: http.StatusOK, Body: map[string]any{"hits": out}}, nil
}

// recordPath builds a node's canonical path from its record.
func recordPath(r store.Record) string {
	doc := "/documents/" + r.Doc
	switch model.Kind(r.Kind) {
	case model.KindScene:
		return doc + "/scenes/" + r.ID
	case model.KindElement:
		return doc + "/scenes/" + r.Parent + "/elements/" + r.Type + "/" + r.ID
	case model.KindCharacter:
		return doc + "/characters/" + r.ID
	}
	return doc
}
