package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/order"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
)

// decodeJSON reads a request body, keeping numbers as written.
func decodeJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, badRequest("malformed JSON: %v", err)
	}
	if dec.More() {
		return nil, badRequest("malformed JSON: more than one value")
	}
	return v, nil
}

func decodeObject(body []byte) (map[string]any, error) {
	v, err := decodeJSON(body)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badRequest("the body must be a JSON object")
	}
	return m, nil
}

// mergePatch applies an RFC 7396 merge patch to a copy of target.
func mergePatch(target any, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	out := map[string]any{}
	if ok {
		for k, v := range t {
			out[k] = v
		}
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

// subrouteFor names the route that owns a field, for use_subroute answers.
func subrouteFor(r *paths.Route, field string) string {
	for _, f := range r.SetFields {
		if f == field {
			return r.Pattern + "/" + field
		}
	}
	return r.Pattern + "/…" + field
}

// checkOwned refuses a PATCH or PUT that would change a field with a route of
// its own (R-PATH-05, R-DER-01). A field sent with the value it already has is
// not a change, so a client can PUT back what it read.
func checkOwned(r *paths.Route, body map[string]any, current any, allowChildren bool) error {
	cur, _ := current.(map[string]any)
	for _, f := range r.Owned() {
		v, present := body[f]
		if !present {
			continue
		}
		if allowChildren && (f == "body" || f == "notes" || f == "revisions") {
			continue
		}
		if old, ok := cur[f]; ok && jsonEqual(old, v) {
			continue
		}
		e := newErr(http.StatusUnprocessableEntity, CodeUseSubroute,
			"%s can't be changed here; it has its own route", f)
		e.Extra = map[string]any{"route": subrouteFor(r, f)}
		return e
	}
	return nil
}

// treeTarget is the tree node a write to a location changes, for checkout and
// lock checks.
func (l *loc) treeTarget() *model.Node { return l.owner }

// Get answers GET on a node route.
func (m *Manager) Get(ctx context.Context, req *Request) (*Response, error) {
	t, err := m.Tree(ctx, req.Target.Doc())
	if err != nil {
		return nil, err
	}
	l, err := locate(t, req.Target)
	if err != nil {
		return nil, err
	}
	v, err := l.value()
	if err != nil {
		return nil, err
	}
	if l.isTreeNode() && l.owner.Kind == model.KindDocument {
		v = rootView(v, req)
	}
	return nodeResponse(http.StatusOK, t, l.revNode, v), nil
}

// rootView applies ?include=analysis and ?view=outline to a whole document
// (R-DOC-02, R-DOC-03). The top-level maps it changes are fresh from JSON.
func rootView(v any, req *Request) any {
	doc, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if req.Query.Get("include") != "analysis" {
		delete(doc, "analysis")
	}
	if req.Query.Get("view") == "outline" {
		if wrapper, ok := doc["document"].(map[string]any); ok {
			w := make(map[string]any, len(wrapper))
			for k, x := range wrapper {
				w[k] = x
			}
			scenes, _ := w["scenes"].([]any)
			out := make([]any, len(scenes))
			for i, s := range scenes {
				sm, _ := s.(map[string]any)
				cp := make(map[string]any, len(sm))
				for k, x := range sm {
					cp[k] = x
				}
				cp["body"] = []any{}
				out[i] = cp
			}
			w["scenes"] = out
			doc["document"] = w
		}
	}
	return doc
}

// mutable returns the object at a location in the clone, ready to change, and
// records the change. For a tree node it is the node's Fields. create makes a
// missing field object (a PATCH or PUT of an optional object that isn't there
// yet).
func (w *write) mutable(l *loc, create bool) (map[string]any, error) {
	if l.isTreeNode() {
		return w.edit(l.owner).Fields, nil
	}
	w.edit(l.owner)
	fields := l.owner.Fields
	if l.key != "" {
		v, _ := walkPath(fields, l.path)
		list, _ := v.([]any)
		for i, item := range list {
			if m, ok := item.(map[string]any); ok && itemKey(m) == l.key {
				if l.anchor != l.owner {
					w.touchEmbedded(l.anchor.ID).fields = true
				}
				list[i] = m
				return m, nil
			}
		}
		return nil, notFound("no %s %q", l.route.Kind, l.key)
	}
	v, ok := walkPath(fields, l.path)
	if !ok {
		if !create {
			return nil, notFound("%s has no %s", l.owner, strings.Join(l.path, "."))
		}
		m := map[string]any{}
		setPath(fields, l.path, m)
		return m, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badRequest("%s is not an object", strings.Join(l.path, "."))
	}
	return m, nil
}

// replaceObject puts a new object at a location in the clone.
func (w *write) replaceObject(l *loc, obj map[string]any) error {
	w.edit(l.owner)
	fields := l.owner.Fields
	if l.key != "" {
		v, _ := walkPath(fields, l.path)
		list, _ := v.([]any)
		for i, item := range list {
			if m, ok := item.(map[string]any); ok && itemKey(m) == l.key {
				list[i] = obj
				if l.anchor != l.owner {
					w.touchEmbedded(l.anchor.ID).fields = true
				}
				return nil
			}
		}
		return notFound("no %s %q", l.route.Kind, l.key)
	}
	setPath(fields, l.path, obj)
	return nil
}

// Patch answers PATCH on a node route: an RFC 7396 merge patch limited by the
// route's owned fields (R-API-09, R-PATH-05).
func (m *Manager) Patch(ctx context.Context, req *Request) (*Response, error) {
	patch, err := decodeObject(req.Body)
	if err != nil {
		return nil, err
	}
	tg := req.Target
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		l, err := locate(w.t, tg)
		if err != nil {
			return nil, err
		}
		current, _ := l.value()
		if err := checkOwned(tg.Route, patch, current, false); err != nil {
			return nil, err
		}
		if err := m.precondition(req, l.revNode.Env.Rev, false); err != nil {
			return nil, err
		}
		if err := m.co.check(w.t.Doc(), l.treeTarget(), false, req.Token); err != nil {
			return nil, err
		}
		if id, ok := patch["id"]; ok {
			cur, _ := l.value()
			if cm, _ := cur.(map[string]any); cm != nil && id != cm["id"] {
				return nil, invalidf("/id", "an id can't be changed")
			}
		}

		if l.isTreeNode() {
			n := w.edit(l.owner)
			if n.Kind == model.KindElement {
				delete(patch, "scene") // the server sets it (R-API-16)
			}
			merged := mergePatch(n.Fields, patch).(map[string]any)
			n.Fields = merged
			w.t.Owns(n)
			if n.Kind == model.KindElement {
				typ, _ := merged["type"].(string)
				if model.ElementType(typ) != n.Type {
					t, err := model.ParseElementType(typ)
					if err != nil {
						return nil, invalidf("/type", "%v", err)
					}
					n.Type = t
				}
			}
		} else {
			obj, err := w.mutable(l, true)
			if err != nil {
				return nil, err
			}
			merged := mergePatch(obj, patch).(map[string]any)
			if err := w.replaceObject(l, merged); err != nil {
				return nil, err
			}
		}
		w.afterWrite(req, l)
		return w.nodeReply(l, http.StatusOK), nil
	})
}

// afterWrite applies ?pass= and ?release= once a write to a node commits
// (R-CO-07, R-CO-09).
func (w *write) afterWrite(req *Request, l *loc) {
	label := req.Query.Get("pass")
	release := req.Query.Get("release") == "true"
	if label == "" && !release {
		return
	}
	n := l.anchor
	id := checkoutID(n)
	doc := w.t.Doc()
	w.afterCommit = append(w.afterCommit, func() {
		if label != "" {
			// In memory now, before the release below; on disk after the
			// queue moves on, but before the caller hears back.
			if err := w.m.passes.markMemory(label, id); err != nil {
				w.m.o.Log.Error("mark pass progress", "pass", label, "node", id, "err", err)
			}
			w.later = append(w.later, func() {
				if err := w.m.passes.persist(label, id); err != nil {
					w.m.o.Log.Error("save pass progress", "pass", label, "node", id, "err", err)
				}
			})
		}
		if release && req.Token != "" {
			if co := w.m.co.release(doc, id, req.Token); co != nil {
				w.m.announce(co, true)
			}
		}
	})
}

// nodeReply renders a location after the write commits. A tree node that
// changed type answers with its new Location (R-API-09).
func (w *write) nodeReply(l *loc, status int) replyFunc {
	return func() *Response {
		rev := l.revNode
		if l.anchor != l.owner && !l.isTreeNode() && isEmbedded(l.anchor) {
			if n := w.t.Node(l.anchor.ID); n != nil {
				rev = n
			}
		}
		v, err := l.value()
		if err != nil {
			v = nil
		}
		r := nodeResponse(status, w.t, rev, v)
		if l.isTreeNode() && l.owner.Kind == model.KindDocument {
			r.Body = rootView(v, w.req)
		}
		if l.isTreeNode() {
			if p, err := paths.For(l.owner); err == nil && l.owner.Kind == model.KindElement {
				if old := w.cur.Node(l.owner.ID); old != nil && old.Type != l.owner.Type {
					r.Location = p
				}
			}
		}
		return r
	}
}

// Put answers PUT on a node route: the node is replaced. A scene's PUT may
// carry its body, which replaces its elements, keeping IDs where they match
// (R-API-10).
func (m *Manager) Put(ctx context.Context, req *Request) (*Response, error) {
	body, err := decodeObject(req.Body)
	if err != nil {
		return nil, err
	}
	tg := req.Target
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		l, err := locate(w.t, tg)
		if err != nil {
			return nil, err
		}
		current, _ := l.value()
		if err := checkOwned(tg.Route, body, current, true); err != nil {
			return nil, err
		}
		if err := m.precondition(req, l.revNode.Env.Rev, false); err != nil {
			return nil, err
		}
		structural := l.isTreeNode()
		if err := m.co.check(w.t.Doc(), l.treeTarget(), structural, req.Token); err != nil {
			return nil, err
		}
		if cur, err := l.value(); err == nil {
			if cm, _ := cur.(map[string]any); cm != nil {
				if id, ok := body["id"]; ok && cm["id"] != nil && id != cm["id"] {
					return nil, invalidf("/id", "an id can't be changed")
				}
				if _, ok := body["id"]; !ok && cm["id"] != nil {
					body["id"] = cm["id"]
				}
				if a, ok := cm["authority"]; ok {
					if _, set := body["authority"]; !set {
						body["authority"] = a
					}
				}
			}
		}
		if !l.isTreeNode() {
			if err := w.replaceObject(l, body); err != nil {
				return nil, err
			}
			return w.nodeReply(l, http.StatusOK), nil
		}
		if err := w.replaceTreeNode(l.owner, body); err != nil {
			return nil, err
		}
		return w.nodeReply(l, http.StatusOK), nil
	})
}

// replaceTreeNode swaps a scene, element or character for a new version built
// from body. Children whose IDs match keep their envelopes; the rest are new or
// removed.
func (w *write) replaceTreeNode(n *model.Node, body map[string]any) error {
	if lk := lockedWithin(n); lk != nil {
		return lockedErr(lk)
	}
	if n.Kind == model.KindElement {
		body["scene"] = n.Parent.ID
	}
	if n.Kind == model.KindScene {
		if _, ok := body["body"]; !ok {
			// No body given: the elements stay as they are.
			kids := make([]any, 0, len(n.Body()))
			for _, el := range n.Body() {
				v, err := el.JSON()
				if err != nil {
					return err
				}
				kids = append(kids, v)
			}
			body["body"] = kids
		}
		for _, item := range asList(body["body"]) {
			if el, ok := item.(map[string]any); ok {
				el["scene"] = n.ID
				if _, has := el["id"]; !has {
					el["id"] = w.m.ids.next()
				}
			}
		}
	}
	fresh, err := model.Build(n.Kind, body)
	if err != nil {
		return invalidf("", "%v", err)
	}
	w.t.Own(n)
	w.t.Owns(n)
	n.Fields, n.Type = fresh.Fields, fresh.Type
	w.touch(n).fields = true
	if n.Kind != model.KindScene {
		return nil
	}

	old := map[string]*model.Node{}
	for _, el := range n.Body() {
		old[el.ID] = el
	}
	kids := fresh.Body()
	keep := make([]*model.Node, 0, len(kids))
	for _, k := range kids {
		k.Parent = n
		w.t.Owns(k)
		if prev, ok := old[k.ID]; ok {
			k.Env = prev.Env
			delete(old, k.ID)
			if !reflect.DeepEqual(prev.Fields, k.Fields) || prev.Type != k.Type {
				w.touch(k).fields = true
			}
		} else {
			c := w.touch(k)
			c.inserted, c.fields = true, true
			w.undeleted = append(w.undeleted, k.ID)
		}
		keep = append(keep, k)
	}
	for _, gone := range old {
		w.cleanupRemoved(gone)
		w.removeRecord(gone)
		w.deleted = append(w.deleted, gone.ID)
		w.event(w.nodeEvent(OpRemoved, gone))
	}
	n.SetChildren("body", keep)
	if err := w.rekeyIfNeeded(n, "body"); err != nil {
		return err
	}
	w.markList(n)
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// rekeyIfNeeded gives a child list valid, strictly increasing order keys,
// keeping existing keys where they already are, and re-keying the whole list
// evenly when they aren't (R-STORE-11).
func (w *write) rekeyIfNeeded(parent *model.Node, kids string) error {
	list := parent.Child(kids)
	ok := true
	keys := make([]string, len(list))
	for i, k := range list {
		keys[i] = k.Env.Order
		if !order.Valid(k.Env.Order) || (i > 0 && keys[i-1] >= k.Env.Order) {
			ok = false
		}
	}
	if ok && !order.NeedsRekey(keys) {
		return nil
	}
	spread, err := order.Spread(len(list))
	if err != nil {
		return err
	}
	for i, k := range list {
		if k.Env.Order != spread[i] {
			k.Env.Order = spread[i]
			w.touch(k)
		}
	}
	w.touch(parent).list = true
	return nil
}

// Delete answers DELETE on a node route (SPEC.md 6.6, 7.3).
func (m *Manager) Delete(ctx context.Context, req *Request) (*Response, error) {
	tg := req.Target
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		l, err := locate(w.t, tg)
		if err != nil {
			return nil, err
		}
		if err := m.precondition(req, l.revNode.Env.Rev, false); err != nil {
			return nil, err
		}
		if err := m.co.check(w.t.Doc(), l.treeTarget(), l.isTreeNode(), req.Token); err != nil {
			return nil, err
		}
		counts := map[string]int{}
		switch {
		case l.isTreeNode():
			if err := w.deleteTreeNode(l.owner, counts); err != nil {
				return nil, err
			}
		case l.key != "":
			if err := w.deleteItem(l); err != nil {
				return nil, err
			}
		default:
			fields := w.edit(l.owner).Fields
			if !deletePath(fields, l.path) {
				return nil, notFound("%s has no %s", l.owner, strings.Join(l.path, "."))
			}
		}
		return func() *Response {
			r := &Response{Status: http.StatusNoContent, Seq: w.t.Seq()}
			if len(counts) > 0 {
				r.Header = map[string]string{"ScreenJSON-Removed": removedHeader(counts)}
			}
			return r
		}, nil
	})
}

// removedHeader writes ScreenJSON-Removed (R-VAL-07).
func removedHeader(counts map[string]int) string {
	var parts []string
	for _, k := range []string{"bookmarks", "embeddings", "passages", "summaries"} {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// deleteTreeNode removes a scene, element, character or analysis.
func (w *write) deleteTreeNode(n *model.Node, counts map[string]int) error {
	if lk := lockedWithin(n); lk != nil {
		return lockedErr(lk)
	}
	switch n.Kind {
	case model.KindDocument:
		return badRequest("delete a document at /documents/{doc}")
	case model.KindAnalysis:
		w.t.Root.SetChildren("analysis", nil)
		delete(w.t.Root.Kids, "analysis")
		w.touch(w.t.Root)
		w.removeRecord(n)
		w.touch(n).skipVal = true
		n.Parent = nil
		return nil
	case model.KindCharacter:
		if refs := referencing(w.t, n.ID, model.KindCharacter); len(refs) > 0 {
			return referenceErr(n.ID, refs)
		}
	case model.KindScene:
		if len(w.t.Scenes()) <= 1 {
			return invalidf("/document/scenes", "a document needs at least one scene, so its last scene can't be deleted")
		}
	}
	parent := n.Parent
	w.cleanupRemoved(n, counts)
	_ = n.Walk(func(x *model.Node) error {
		w.removeRecord(x)
		w.deleted = append(w.deleted, x.ID)
		return nil
	})
	w.event(w.nodeEvent(OpRemoved, n))
	kids := kidsOf[n.Kind]
	list := parent.Child(kids)
	keep := make([]*model.Node, 0, len(list))
	for _, k := range list {
		if k != n {
			keep = append(keep, k)
		}
	}
	parent.SetChildren(kids, keep)
	n.Parent = nil
	w.markList(parent)
	return nil
}

func referenceErr(id string, refs []string) *Error {
	e := newErr(http.StatusConflict, CodeReference, "%s is still referenced by %d node(s)", id, len(refs))
	e.Extra = map[string]any{"ids": refs}
	return e
}

// cleanupRemoved removes what points at a deleted scene or element and its
// descendants (R-VAL-07): bookmarks, embeddings, passages, and summaries that
// target it.
func (w *write) cleanupRemoved(n *model.Node, counts ...map[string]int) {
	c := map[string]int{}
	if len(counts) > 0 {
		c = counts[0]
	}
	ids := map[string]bool{}
	_ = n.Walk(func(x *model.Node) error {
		ids[x.ID] = true
		return nil
	})
	for id := range ids {
		c["embeddings"] += w.dropEmbeddings(id)
	}
	c["passages"] += w.dropPassages(func(p map[string]any) bool {
		if ids[fmt.Sprint(p["scene"])] {
			return true
		}
		for _, e := range stringList(p["elements"]) {
			if ids[e] {
				return true
			}
		}
		return false
	})
	c["summaries"] += w.dropSummaries(func(s map[string]any) bool { return ids[fmt.Sprint(s["target"])] })
	c["bookmarks"] += w.dropBookmarks(func(b map[string]any) bool {
		return ids[fmt.Sprint(b["scene"])] || ids[fmt.Sprint(b["element"])]
	})
}

// dropBookmarks removes bookmarks matching a rule.
func (w *write) dropBookmarks(match func(map[string]any) bool) int {
	wrapper, _ := w.t.Root.Fields["document"].(map[string]any)
	list, _ := wrapper["bookmarks"].([]any)
	hit := false
	for _, item := range list {
		if b, ok := item.(map[string]any); ok && match(b) {
			hit = true
		}
	}
	if !hit {
		return 0
	}
	root := w.edit(w.t.Root)
	wrapper = root.Fields["document"].(map[string]any)
	list = wrapper["bookmarks"].([]any)
	kept := make([]any, 0, len(list))
	for _, item := range list {
		if b, ok := item.(map[string]any); ok && match(b) {
			if id, _ := b["id"].(string); id != "" {
				w.deleted = append(w.deleted, id)
			}
			continue
		}
		kept = append(kept, item)
	}
	wrapper["bookmarks"] = kept
	return len(list) - len(kept)
}

// deleteItem removes an item from its array, refusing to remove an author,
// contributor or source that something still refers to (R-VAL-06).
func (w *write) deleteItem(l *loc) error {
	kind := l.route.Kind
	if kind == "" {
		kind = l.anchor.Kind
	}
	switch kind {
	case model.KindAuthor, model.KindContributor, model.KindSource:
		if refs := referencing(w.t, l.key, kind); len(refs) > 0 {
			return referenceErr(l.key, refs)
		}
	}
	w.edit(l.owner)
	i, list, err := l.index()
	if err != nil {
		return err
	}
	list = append(list[:i:i], list[i+1:]...)
	setPath(l.owner.Fields, l.path, list)
	if isEmbedded(l.anchor) {
		w.deleted = append(w.deleted, l.anchor.ID)
	}
	return nil
}
