package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/order"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
)

// kindOfKids is the kind of node each child list holds.
var kindOfKids = map[string]model.Kind{
	"scenes":     model.KindScene,
	"body":       model.KindElement,
	"characters": model.KindCharacter,
}

// position is where an insert or move goes (SPEC.md 6.5, 6.7).
type position struct {
	after, before, where string
}

func positionFrom(q url.Values) (position, error) {
	p := position{after: q.Get("after"), before: q.Get("before"), where: q.Get("position")}
	return p, p.check()
}

func (p position) check() error {
	n := 0
	for _, v := range []string{p.after, p.before, p.where} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return badRequest("give only one of after, before or position")
	}
	if p.where != "" && p.where != "first" && p.where != "last" {
		return badRequest("position must be first or last")
	}
	return nil
}

// index finds where in a list of keys the position points. keys are the
// siblings' identifiers in order. The default is last (R-API-13).
func (p position) index(keys []string) (int, error) {
	find := func(id string) (int, error) {
		for i, k := range keys {
			if k == id {
				return i, nil
			}
		}
		return 0, invalidf("", "sibling %s is not in this list", id)
	}
	switch {
	case p.after != "":
		i, err := find(p.after)
		return i + 1, err
	case p.before != "":
		return find(p.before)
	case p.where == "first":
		return 0, nil
	}
	return len(keys), nil
}

// ListGet answers GET on a list inside one document: a plain array in
// document order, with the parent's crev as ETag (R-API-03).
func (m *Manager) ListGet(ctx context.Context, req *Request) (*Response, error) {
	t, err := m.Tree(ctx, req.Target.Doc())
	if err != nil {
		return nil, err
	}
	l, err := locateList(t, req.Target)
	if err != nil {
		return nil, err
	}
	items, err := l.items()
	if err != nil {
		return nil, err
	}
	crev := uint64(0)
	if l.owner != nil {
		crev = l.owner.Env.CRev
	}
	return &Response{Status: http.StatusOK, Body: items, ETag: etag(crev, true), Seq: t.Seq()}, nil
}

// Insert answers POST on a list (SPEC.md 6.7).
func (m *Manager) Insert(ctx context.Context, req *Request) (*Response, error) {
	body, err := decodeObject(req.Body)
	if err != nil {
		return nil, err
	}
	pos, err := positionFrom(req.Query)
	if err != nil {
		return nil, err
	}
	tg := req.Target
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		l, err := locateList(w.t, tg)
		if err != nil {
			return nil, err
		}
		crev := uint64(0)
		target := l.anchor
		if l.owner != nil {
			crev, target = l.owner.Env.CRev, l.owner
		}
		if err := m.precondition(req, crev, true); err != nil {
			return nil, err
		}
		if err := m.co.check(w.t.Doc(), target, false, req.Token); err != nil {
			return nil, err
		}
		if lk := lockedWithin(target); lk != nil && target.Kind == model.KindElement {
			return nil, lockedErr(lk)
		}
		if l.kids != "" {
			return w.insertTreeNode(l, body, pos)
		}
		return w.insertItem(l, body, pos)
	})
}

// fillAuthors applies R-API-15: missing authors come from the previous
// sibling, else the parent, else (for a scene) the root's first author.
func (w *write) fillAuthors(body map[string]any, parent *model.Node, prev *model.Node) {
	if _, ok := body["authors"]; ok {
		return
	}
	if prev != nil {
		if a, ok := prev.Fields["authors"]; ok {
			body["authors"] = deepCopy(a)
			return
		}
	}
	if parent.Kind == model.KindScene {
		if a, ok := parent.Fields["authors"]; ok {
			body["authors"] = deepCopy(a)
			return
		}
	}
	authors, _ := w.t.Root.Fields["authors"].([]any)
	if len(authors) > 0 {
		if first, ok := authors[0].(map[string]any); ok {
			if id, ok := first["id"].(string); ok {
				body["authors"] = []any{id}
			}
		}
	}
}

func deepCopy(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	out, _ := decodeJSON(b)
	return out
}

// insertTreeNode inserts a scene, element or character.
func (w *write) insertTreeNode(l *listLoc, body map[string]any, pos position) (replyFunc, error) {
	parent := l.owner
	kind := kindOfKids[l.kids]
	if kind == model.KindElement && l.typ != "" {
		if t, ok := body["type"]; ok && t != string(l.typ) {
			return nil, invalidf("/type", "the body's type %v differs from the path's %s", t, l.typ)
		}
		body["type"] = string(l.typ)
	}
	siblings := parent.Child(l.kids)
	ids := make([]string, len(siblings))
	for i, s := range siblings {
		ids[i] = s.ID
	}
	at, err := pos.index(ids)
	if err != nil {
		return nil, err
	}
	var prev, next *model.Node
	if at > 0 {
		prev = siblings[at-1]
	}
	if at < len(siblings) {
		next = siblings[at]
	}

	if kind == model.KindElement {
		body["scene"] = parent.ID // R-API-16
	}
	if kind == model.KindScene || kind == model.KindElement {
		w.fillAuthors(body, parent, prev)
	}
	id, hasID := body["id"].(string)
	if !hasID || id == "" {
		id = w.m.ids.next()
		body["id"] = id
	}
	if kind == model.KindScene {
		for _, item := range asList(body["body"]) {
			el, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := el["id"].(string); !ok {
				el["id"] = w.m.ids.next()
			}
			el["scene"] = id
			if _, ok := el["authors"]; !ok {
				if a, ok := body["authors"]; ok {
					el["authors"] = deepCopy(a)
				}
			}
		}
		if _, ok := body["body"]; !ok {
			body["body"] = []any{}
		}
	}

	// R-API-14: a client-supplied ID already in use is a retry when the
	// content is identical, and a conflict otherwise.
	if existing := w.t.Node(id); existing != nil || (w.t.Root.ID == id) {
		if existing != nil && existing.Kind == kind {
			cur, err := existing.JSON()
			if err == nil && jsonEqual(cur, body) {
				w.noop = true
				return func() *Response {
					r := nodeResponse(http.StatusOK, w.cur, w.cur.Node(id), cur)
					r.Location, _ = paths.For(w.cur.Node(id))
					return r
				}, nil
			}
		}
		return nil, newErr(http.StatusConflict, CodeDuplicateID, "id %s is already used in this document with different content", id)
	}

	n, err := model.Build(kind, body)
	if err != nil {
		return nil, invalidf("", "%v", err)
	}
	w.t.Owns(n)
	key, err := orderBetween(prev, next)
	if err != nil {
		return nil, err
	}
	n.Env.Order = key
	list := make([]*model.Node, 0, len(siblings)+1)
	list = append(list, siblings[:at]...)
	list = append(list, n)
	list = append(list, siblings[at:]...)
	parent.SetChildren(l.kids, list)
	c := w.touch(n)
	c.inserted, c.fields = true, true
	w.undeleted = append(w.undeleted, id)
	if kind == model.KindScene {
		keys, err := order.Spread(len(n.Body()))
		if err != nil {
			return nil, err
		}
		for i, el := range n.Body() {
			w.t.Owns(el)
			el.Env.Order = keys[i]
			ec := w.touch(el)
			ec.inserted, ec.fields = true, true
			w.undeleted = append(w.undeleted, el.ID)
		}
		w.bodyChanged[n] = true
	}
	if err := w.rekeyIfNeeded(parent, l.kids); err != nil {
		return nil, err
	}
	w.markList(parent)
	ev := w.nodeEvent(OpInserted, n)
	ev.After = afterID(prev)
	w.event(ev)

	return func() *Response {
		v, _ := n.JSON()
		r := nodeResponse(http.StatusCreated, w.t, n, v)
		r.Location, _ = paths.For(n)
		return r
	}, nil
}

func afterID(prev *model.Node) *string {
	if prev == nil {
		return nil
	}
	id := prev.ID
	return &id
}

// orderBetween makes a key between two siblings.
func orderBetween(prev, next *model.Node) (string, error) {
	a, b := "", ""
	if prev != nil {
		a = prev.Env.Order
	}
	if next != nil {
		b = next.Env.Order
	}
	if a != "" && b != "" && a >= b {
		// The neighbours' keys are out of order; rekeyIfNeeded will fix the
		// list, so any valid key does for now.
		return order.Between(a, "")
	}
	return order.Between(a, b)
}

// insertItem inserts an object into an array: an author, note, color,
// passage and so on.
func (w *write) insertItem(l *listLoc, body map[string]any, pos position) (replyFunc, error) {
	kind := l.route.Kind
	if l.owner == nil {
		// A passage or summary on a document with no analysis yet.
		l.owner = w.analysis(true)
	}
	usesUUID := kind.HasUUID()
	switch kind {
	case model.KindStyle, model.KindTemplate, model.KindGuide:
		usesUUID = false
	}
	if usesUUID {
		if _, ok := body["id"].(string); !ok {
			body["id"] = w.m.ids.next()
		}
	}
	key := itemKey(body)
	if key == "" || (kind == model.KindRegistration && body["authority"] == nil) {
		return nil, invalidf("/id", "a %s needs an id", kind)
	}
	if usesUUID && w.t.Node(key) != nil {
		existing := w.t.Node(key)
		if existing.Kind == kind && jsonEqual(existing.Fields, body) {
			w.noop = true
			return w.itemReply(l, key, http.StatusOK, true), nil
		}
		return nil, newErr(http.StatusConflict, CodeDuplicateID, "id %s is already used in this document with different content", key)
	}
	if kind == model.KindNote {
		if _, ok := body["created"]; !ok {
			body["created"] = w.now.Format("2006-01-02T15:04:05Z07:00")
		}
	}

	fields := w.edit(l.owner).Fields
	v, _ := walkPath(fields, l.path)
	items := asList(v)
	keys := make([]string, len(items))
	for i, item := range items {
		if m, ok := item.(map[string]any); ok {
			keys[i] = itemKey(m)
			if keys[i] == key {
				if jsonEqual(m, body) {
					w.noop = true
					return w.itemReply(l, key, http.StatusOK, true), nil
				}
				return nil, newErr(http.StatusConflict, CodeDuplicateID, "%s %q already exists with different content", kind, key)
			}
		}
	}
	at, err := pos.index(keys)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(items)+1)
	out = append(out, items[:at]...)
	out = append(out, body)
	out = append(out, items[at:]...)
	setPath(fields, l.path, out)
	if usesUUID {
		w.touchEmbedded(key).inserted = true
		w.undeleted = append(w.undeleted, key)
	}
	return w.itemReply(l, key, http.StatusCreated, false), nil
}

// itemReply renders an inserted item with its Location.
func (w *write) itemReply(l *listLoc, key string, status int, fromCur bool) replyFunc {
	return func() *Response {
		t := w.t
		if fromCur {
			t = w.cur
		}
		base, err := paths.For(l.anchor)
		if err == nil {
			base += routeTail(l.route)
		}
		loc := base + "/" + url.PathEscape(key)
		rev := l.owner
		if n := t.Node(key); n != nil {
			rev = n
			if p, err := paths.For(n); err == nil {
				loc = p
			}
		}
		var body any
		v, _ := walkPath(ownerIn(t, l.owner).Fields, l.path)
		for _, item := range asList(v) {
			if m, ok := item.(map[string]any); ok && itemKey(m) == key {
				body = m
			}
		}
		r := nodeResponse(status, t, rev, body)
		r.Location = loc
		return r
	}
}

// ownerIn finds a tree node's counterpart in another version of the tree.
func ownerIn(t *model.Tree, n *model.Node) *model.Node {
	switch n.Kind {
	case model.KindDocument:
		return t.Root
	case model.KindAnalysis:
		if a := t.Root.Analysis(); a != nil {
			return a
		}
		return n
	}
	if x := t.Node(n.ID); x != nil {
		return x
	}
	return n
}

// routeTail is the part of a route's pattern after its anchor parameter.
func routeTail(r *paths.Route) string {
	marker := "{" + r.Anchor + "}"
	i := strings.LastIndex(r.Pattern, marker)
	if i < 0 {
		return ""
	}
	return r.Pattern[i+len(marker):]
}

// moveBody is the body of POST …/move (R-PATH-09).
type moveBody struct {
	Scene    string `json:"scene"`
	After    string `json:"after"`
	Before   string `json:"before"`
	Position string `json:"position"`
}

// Move answers POST {scene or element}/move.
func (m *Manager) Move(ctx context.Context, req *Request) (*Response, error) {
	var b moveBody
	if err := json.Unmarshal(req.Body, &b); err != nil {
		return nil, badRequest("move body: %v", err)
	}
	pos := position{after: b.After, before: b.Before, where: b.Position}
	given := 0
	for _, v := range []string{b.After, b.Before, b.Position} {
		if v != "" {
			given++
		}
	}
	if given != 1 {
		return nil, badRequest("give exactly one of after, before or position")
	}
	if err := pos.check(); err != nil {
		return nil, err
	}
	tg := req.Target
	return m.submit(ctx, tg.Doc(), req, func(w *write) (replyFunc, error) {
		n, err := resolve(w.t, tg)
		if err != nil {
			return nil, err
		}
		if err := m.precondition(req, n.Env.Rev, false); err != nil {
			return nil, err
		}
		if lk := lockedWithin(n); lk != nil {
			return nil, lockedErr(lk)
		}
		if err := m.co.check(w.t.Doc(), n, true, req.Token); err != nil {
			return nil, err
		}
		oldParent := n.Parent
		newParent := oldParent
		if n.Kind == model.KindElement && b.Scene != "" && b.Scene != oldParent.ID {
			target := w.t.Node(b.Scene)
			switch {
			case target == nil && w.t.WasDeleted(b.Scene):
				return nil, invalidf("/scene", "scene %s was deleted", b.Scene)
			case target == nil || target.Kind != model.KindScene:
				return nil, invalidf("/scene", "scene %s is not in this document; moving between documents isn't supported", b.Scene)
			}
			if err := m.co.check(w.t.Doc(), target, false, req.Token); err != nil {
				return nil, err
			}
			newParent = target
		} else if n.Kind == model.KindScene && b.Scene != "" {
			return nil, badRequest("scene is only for moving elements")
		}
		oldPath, _ := paths.For(n)
		kids := kidsOf[n.Kind]

		rest := make([]*model.Node, 0, len(newParent.Child(kids)))
		for _, s := range newParent.Child(kids) {
			if s != n {
				rest = append(rest, s)
			}
		}
		ids := make([]string, len(rest))
		for i, s := range rest {
			ids[i] = s.ID
		}
		if (pos.after == n.ID) || (pos.before == n.ID) {
			return nil, invalidf("", "a node can't be moved next to itself")
		}
		at, err := pos.index(ids)
		if err != nil {
			return nil, err
		}
		var prev, next *model.Node
		if at > 0 {
			prev = rest[at-1]
		}
		if at < len(rest) {
			next = rest[at]
		}
		key, err := orderBetween(prev, next)
		if err != nil {
			return nil, err
		}
		if newParent != oldParent {
			keep := make([]*model.Node, 0, len(oldParent.Child(kids)))
			for _, s := range oldParent.Child(kids) {
				if s != n {
					keep = append(keep, s)
				}
			}
			oldParent.SetChildren(kids, keep)
			w.markList(oldParent)
			w.t.Own(n)
			n.Fields["scene"] = newParent.ID
			// R-VAL-08: bookmarks follow the element; passages that include
			// it are no longer continuous and go.
			w.retargetBookmarks(n.ID, newParent.ID)
			w.dropPassages(func(p map[string]any) bool { return passageIncludes(p, n.ID) })
		}
		list := make([]*model.Node, 0, len(rest)+1)
		list = append(list, rest[:at]...)
		list = append(list, n)
		list = append(list, rest[at:]...)
		newParent.SetChildren(kids, list)
		n.Env.Order = key
		w.touch(n).moved = true
		if err := w.rekeyIfNeeded(newParent, kids); err != nil {
			return nil, err
		}
		w.markList(newParent)
		ev := w.nodeEvent(OpMoved, n)
		ev.OldPath = oldPath
		ev.After = afterID(prev)
		w.event(ev)
		return func() *Response {
			v, _ := n.JSON()
			r := nodeResponse(http.StatusOK, w.t, n, v)
			r.Location, _ = paths.For(n)
			return r
		}, nil
	})
}

// retargetBookmarks points bookmarks on an element at its new scene.
func (w *write) retargetBookmarks(elementID, sceneID string) {
	wrapper, _ := w.t.Root.Fields["document"].(map[string]any)
	hit := false
	for _, item := range asList(wrapper["bookmarks"]) {
		if b, ok := item.(map[string]any); ok && b["element"] == elementID {
			hit = true
		}
	}
	if !hit {
		return
	}
	root := w.edit(w.t.Root)
	wrapper = root.Fields["document"].(map[string]any)
	for _, item := range asList(wrapper["bookmarks"]) {
		if b, ok := item.(map[string]any); ok && b["element"] == elementID {
			b["scene"] = sceneID
			if id, ok := b["id"].(string); ok {
				w.touchEmbedded(id).fields = true
			}
		}
	}
}

// WildcardList answers a GET on a list with a "-" document or scene: items
// from many documents, paginated with limit and cursor (R-API-06, R-PATH-03).
func (m *Manager) WildcardList(ctx context.Context, req *Request) (*Response, error) {
	tg := req.Target
	limit := 100
	if s := req.Query.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return nil, badRequest("limit must be a positive whole number")
		}
		limit = min(n, 1000)
	}
	startDoc, skip := "", 0
	if c := req.Query.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil {
			return nil, badRequest("bad cursor")
		}
		d, off, ok := strings.Cut(string(raw), "\x00")
		if !ok {
			return nil, badRequest("bad cursor")
		}
		startDoc = d
		if skip, err = strconv.Atoi(off); err != nil {
			return nil, badRequest("bad cursor")
		}
	}
	var docIDs []string
	if tg.Doc() == paths.Wildcard {
		all, err := m.o.State.DocIDs()
		if err != nil {
			return nil, storageErr(err)
		}
		docIDs = all
	} else {
		docIDs = []string{tg.Doc()}
	}
	items := []any{}
	next := ""
	started := startDoc == ""
	for _, id := range docIDs {
		if !started {
			if id != startDoc {
				continue
			}
			started = true
		} else {
			skip = 0
		}
		t, err := m.Tree(ctx, id)
		if err != nil {
			continue
		}
		docItems := wildcardItems(t, tg)
		for i := skip; i < len(docItems); i++ {
			if len(items) == limit {
				next = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s\x00%d", id, i)))
				break
			}
			items = append(items, docItems[i])
		}
		if next != "" {
			break
		}
	}
	var nextVal any
	if next != "" {
		nextVal = next
	}
	return &Response{Status: http.StatusOK, Body: map[string]any{"items": items, "next": nextVal}}, nil
}

// wildcardItems lists one document's items for a wildcard list route.
func wildcardItems(t *model.Tree, tg *paths.Target) []any {
	var out []any
	sceneID := tg.Param(paths.ParamScene)
	typ := model.ElementType(tg.Param(paths.ParamType))
	add := func(n *model.Node) {
		if v, err := n.JSON(); err == nil {
			out = append(out, v)
		}
	}
	switch tg.Route.Kind {
	case model.KindScene:
		for _, s := range t.Scenes() {
			add(s)
		}
	case model.KindElement:
		for _, s := range t.Scenes() {
			if sceneID != paths.Wildcard && sceneID != "" && s.ID != sceneID {
				continue
			}
			for _, el := range s.Body() {
				if typ == "" || el.Type == typ {
					add(el)
				}
			}
		}
	case model.KindCharacter:
		for _, c := range t.Root.Characters() {
			add(c)
		}
	default:
		// Other document lists: take them from the root.
		if owner, toks, err := splitPointer(t.Root, tg.Route.Pointer); err == nil {
			v, _ := walkPath(owner.Fields, toks)
			out = append(out, asList(v)...)
		}
	}
	return out
}
