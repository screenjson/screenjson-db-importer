package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/screenjson/screenjson-db-importer/internal/layout"
	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
)

// requiredMaps lists language maps a route's object must keep at least one
// language in: DELETE of the last one answers 422 (SPEC.md 6.10).
var requiredMaps = map[string]map[string]bool{
	"document":      {"title": true},
	"cover":         {"title": true},
	"element":       {"text": true},
	"source":        {"title": true},
	"bookmark":      {"title": true},
	"note":          {"text": true},
	"passage":       {"text": true},
	"summary":       {"text": true},
	"layout.header": {"text": true},
	"layout.footer": {"text": true},
}

// langObject returns the object a language route works on, read-only.
func langObject(l *loc) (map[string]any, error) {
	v, err := l.value()
	if err != nil {
		return nil, err
	}
	if l.isTreeNode() {
		return l.owner.Fields, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, notFound("no object here")
	}
	return m, nil
}

// Lang answers GET, PUT and DELETE on …/{field}/{lang} (SPEC.md 6.10).
func (m *Manager) Lang(ctx context.Context, req *Request, method string) (*Response, error) {
	tg := req.Target
	field, lang := tg.Field, tg.Param(paths.ParamLang)
	if method == http.MethodGet {
		t, err := m.Tree(ctx, tg.Doc())
		if err != nil {
			return nil, err
		}
		l, err := locate(t, tg)
		if err != nil {
			return nil, err
		}
		obj, err := langObject(l)
		if err != nil {
			return nil, err
		}
		mp, _ := obj[field].(map[string]any)
		s, ok := mp[lang]
		if !ok {
			return nil, notFound("%s has no %s text in %s", l.revNode, field, lang)
		}
		return nodeResponse(http.StatusOK, t, l.revNode, s), nil
	}

	var value string
	if method == http.MethodPut {
		if err := json.Unmarshal(req.Body, &value); err != nil {
			return nil, badRequest("the body must be a JSON string")
		}
	}
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		l, err := locate(w.t, tg)
		if err != nil {
			return nil, err
		}
		if err := m.precondition(req, l.revNode.Env.Rev, false); err != nil {
			return nil, err
		}
		if err := m.co.check(w.t.Doc(), l.treeTarget(), false, req.Token); err != nil {
			return nil, err
		}
		// R-ENC-02: language routes on encrypted text are refused outright.
		if w.encrypted() || ownEncrypted(l.treeTarget()) {
			return nil, newErr(http.StatusConflict, CodeEncrypted, "%s is encrypted, so its text can't be changed through the server", l.treeTarget())
		}
		obj, err := w.mutable(l, method == http.MethodPut)
		if err != nil {
			return nil, err
		}
		mp, _ := obj[field].(map[string]any)
		if method == http.MethodPut {
			if mp == nil {
				mp = map[string]any{}
			}
			mp[lang] = value
			obj[field] = mp
			return func() *Response {
				return nodeResponse(http.StatusOK, w.t, revAfter(w, l), value)
			}, nil
		}
		if _, ok := mp[lang]; !ok {
			return nil, notFound("no %s text in %s", field, lang)
		}
		delete(mp, lang)
		if len(mp) == 0 {
			if requiredMaps[tg.Route.Name][field] {
				return nil, invalidf("/"+field, "%s is required, so its last language can't be removed", field)
			}
			delete(obj, field)
		}
		return func() *Response { return &Response{Status: http.StatusNoContent, Seq: w.t.Seq()} }, nil
	})
}

// revAfter finds the node whose revision a response reports, after a commit.
func revAfter(w *write, l *loc) *model.Node {
	if isEmbedded(l.anchor) {
		if n := w.t.Node(l.anchor.ID); n != nil {
			return n
		}
	}
	return l.revNode
}

// setBody is POST …/{setField}.
type setBody struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// Set answers GET, PUT and POST on a set field (SPEC.md 6.10). Adds and
// removes are applied inside the write queue, so concurrent writers all win
// (R-API-19).
func (m *Manager) Set(ctx context.Context, req *Request, method string) (*Response, error) {
	tg := req.Target
	field := tg.Field
	if method == http.MethodGet {
		t, err := m.Tree(ctx, tg.Doc())
		if err != nil {
			return nil, err
		}
		l, err := locate(t, tg)
		if err != nil {
			return nil, err
		}
		obj, err := langObject(l)
		if err != nil {
			return nil, err
		}
		list := stringList(obj[field])
		return nodeResponse(http.StatusOK, t, l.revNode, list), nil
	}
	var replace []string
	var change setBody
	switch method {
	case http.MethodPut:
		if err := json.Unmarshal(req.Body, &replace); err != nil {
			return nil, badRequest("the body must be an array of strings")
		}
	case http.MethodPost:
		if err := json.Unmarshal(req.Body, &change); err != nil {
			return nil, badRequest(`the body must be {"add": [...], "remove": [...]}`)
		}
	}
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		l, err := locate(w.t, tg)
		if err != nil {
			return nil, err
		}
		if err := m.precondition(req, l.revNode.Env.Rev, false); err != nil {
			return nil, err
		}
		if err := m.co.check(w.t.Doc(), l.treeTarget(), false, req.Token); err != nil {
			return nil, err
		}
		obj, err := w.mutable(l, false)
		if err != nil {
			return nil, err
		}
		var next []string
		if method == http.MethodPut {
			next = replace
		} else {
			cur := stringList(obj[field])
			drop := map[string]bool{}
			for _, r := range change.Remove {
				drop[r] = true
			}
			seen := map[string]bool{}
			for _, s := range cur {
				if !drop[s] && !seen[s] {
					next = append(next, s)
					seen[s] = true
				}
			}
			for _, a := range change.Add {
				if !drop[a] && !seen[a] {
					next = append(next, a)
					seen[a] = true
				}
			}
		}
		out := make([]any, len(next))
		for i, s := range next {
			out[i] = s
		}
		obj[field] = out
		return func() *Response {
			if next == nil {
				next = []string{}
			}
			return nodeResponse(http.StatusOK, w.t, revAfter(w, l), next)
		}, nil
	})
}

// embeddingsOf returns a node's embeddings in a tree.
func embeddingsOf(t *model.Tree, id string) []any {
	a := t.Root.Analysis()
	if a == nil {
		return nil
	}
	emb, _ := a.Fields["embeddings"].(map[string]any)
	return asList(emb[id])
}

func embeddable(n *model.Node) bool {
	switch n.Kind {
	case model.KindScene, model.KindElement, model.KindCharacter:
		return true
	}
	return false
}

// Embeddings answers the embedding routes (SPEC.md 6.11).
func (m *Manager) Embeddings(ctx context.Context, req *Request, method string) (*Response, error) {
	tg := req.Target
	modelName := tg.Param(paths.ParamModel)
	if method == http.MethodGet {
		t, n, err := m.resolveNode(ctx, tg)
		if err != nil {
			return nil, err
		}
		if !embeddable(n) {
			return nil, notFound("%s has no embeddings", n.Kind)
		}
		list := embeddingsOf(t, n.ID)
		r := nodeResponse(http.StatusOK, t, n, nil)
		r.ETag = ""
		if tg.Suffix == paths.SuffixEmbeddings {
			if list == nil {
				list = []any{}
			}
			r.Body = list
			return r, nil
		}
		for _, e := range list {
			if em, ok := e.(map[string]any); ok && em["model"] == modelName {
				r.Body = em
				return r, nil
			}
		}
		return nil, notFound("%s has no embedding for model %s", n.ID, modelName)
	}

	var body map[string]any
	if method == http.MethodPut {
		var err error
		if body, err = decodeObject(req.Body); err != nil {
			return nil, err
		}
	}
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		n, err := resolve(w.t, tg)
		if err != nil {
			return nil, err
		}
		if !embeddable(n) {
			return nil, notFound("%s has no embeddings", n.Kind)
		}
		// R-EMB-03: If-Match here is the node's text_rev, so a vector made
		// from old text is refused.
		if err := m.precondition(req, n.Env.TextRev, false); err != nil {
			return nil, err
		}
		if err := m.co.check(w.t.Doc(), n, false, req.Token); err != nil {
			return nil, err
		}
		if method == http.MethodDelete {
			a := w.t.Root.Analysis()
			found := false
			for _, e := range embeddingsOf(w.t, n.ID) {
				if em, ok := e.(map[string]any); ok && em["model"] == modelName {
					found = true
				}
			}
			if a == nil || !found {
				return nil, notFound("%s has no embedding for model %s", n.ID, modelName)
			}
			a = w.edit(a)
			w.touch(a).skipVal = true
			emb := a.Fields["embeddings"].(map[string]any)
			var kept []any
			for _, e := range asList(emb[n.ID]) {
				if em, ok := e.(map[string]any); ok && em["model"] == modelName {
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(emb, n.ID)
			} else {
				emb[n.ID] = kept
			}
			w.touchVectorOwner(n.ID)
			return func() *Response {
				r := &Response{Status: http.StatusNoContent, Seq: w.t.Seq()}
				r.TextRev, r.HasTextRev = n.Env.TextRev, true
				return r
			}, nil
		}

		e, err := w.fillEmbedding(n, modelName, body)
		if err != nil {
			return nil, err
		}
		a := w.analysis(true)
		w.touch(a).skipVal = true
		emb, _ := a.Fields["embeddings"].(map[string]any)
		if emb == nil {
			emb = map[string]any{}
			a.Fields["embeddings"] = emb
		}
		list := asList(emb[n.ID])
		replaced := false
		for i, item := range list {
			if em, ok := item.(map[string]any); ok && em["model"] == modelName {
				list[i] = e
				replaced = true
			}
		}
		if !replaced {
			list = append(list, e)
		}
		emb[n.ID] = list
		w.touchVectorOwner(n.ID)
		return func() *Response {
			r := &Response{Status: http.StatusOK, Body: e, Seq: w.t.Seq()}
			r.TextRev, r.HasTextRev = n.Env.TextRev, true
			return r
		}, nil
	})
}

// fillEmbedding completes an embedding PUT body (R-EMB-02) and checks it
// (R-EMB-05).
func (w *write) fillEmbedding(n *model.Node, modelName string, e map[string]any) (map[string]any, error) {
	if mv, ok := e["model"]; ok && mv != modelName {
		return nil, invalidf("/model", "the body's model %v differs from the path's %s", mv, modelName)
	}
	e["model"] = modelName
	values, ok := e["values"].([]any)
	if !ok || len(values) == 0 {
		return nil, invalidf("/values", "values must be a non-empty array of numbers")
	}
	if len(values) > layout.MaxDimensions {
		return nil, invalidf("/values", "a vector has at most %d dimensions, not %d", layout.MaxDimensions, len(values))
	}
	if vec := w.m.o.Layout.VectorFor(n.Kind); vec != nil && vec.Model == modelName && vec.Dimensions != len(values) {
		return nil, invalidf("/values", "model %s is stored natively with %d dimensions, not %d", modelName, vec.Dimensions, len(values))
	}
	if d, ok := e["dimensions"]; ok && fmt.Sprint(d) != fmt.Sprint(len(values)) {
		return nil, invalidf("/dimensions", "dimensions is %v but values has %d", d, len(values))
	}
	e["dimensions"] = json.Number(fmt.Sprint(len(values)))
	if _, ok := e["id"]; !ok {
		e["id"] = w.m.ids.next()
	}
	if _, ok := e["created"]; !ok {
		e["created"] = w.now.Format("2006-01-02T15:04:05Z07:00")
	}
	if _, ok := e["source"]; !ok {
		switch n.Kind {
		case model.KindScene:
			e["source"] = "heading"
		case model.KindCharacter:
			e["source"] = "name"
		default:
			e["source"] = "text"
		}
	}
	if s, ok := w.m.o.Schema.Kind("embedding"); ok {
		if d := schema.Validate(s, e); d != nil {
			return nil, invalid("the embedding does not match the schema", d)
		}
	}
	return e, nil
}

// Appearances answers GET …/characters/{id}/appearances (SPEC.md 6.13).
func (m *Manager) Appearances(ctx context.Context, req *Request) (*Response, error) {
	t, c, err := m.resolveNode(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	scenes := []any{}
	elements := []any{}
	for _, s := range t.Scenes() {
		in := false
		for _, el := range s.Body() {
			if el.Fields["character"] != c.ID {
				continue
			}
			in = true
			p, _ := paths.For(el)
			elements = append(elements, map[string]any{"id": el.ID, "scene": s.ID, "type": string(el.Type), "path": p})
		}
		if in {
			h, _ := s.Fields["heading"].(map[string]any)
			scenes = append(scenes, map[string]any{"id": s.ID, "no": h["no"], "heading": layout.Slugline(h)})
		}
	}
	return &Response{Status: http.StatusOK, Body: map[string]any{"scenes": scenes, "elements": elements}, Seq: t.Seq()}, nil
}

// Cast answers GET …/cast: the derived cast, empty when the scene has none.
func (m *Manager) Cast(ctx context.Context, req *Request) (*Response, error) {
	t, s, err := m.resolveNode(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	cast := stringList(s.Fields["cast"])
	return nodeResponse(http.StatusOK, t, s, cast), nil
}
