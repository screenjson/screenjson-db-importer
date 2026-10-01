package docs

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"sync"

	"github.com/screenjson/screenjson-db-importer/internal/layout"
	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/order"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
	"github.com/screenjson/screenjson-db-importer/internal/state"
)

// semver is the schema's version pattern, with the major captured.
var semver = regexp.MustCompile(`^(\d+)\.\d+\.\d+(?:[-+].*)?$`)

// SupportedMajor is the ScreenJSON major version this server reads (R-VAL-04).
const SupportedMajor = 1

// checkVersion refuses documents of another major version with 409.
func checkVersion(doc map[string]any) error {
	v, ok := doc["version"].(string)
	if !ok {
		return nil // the schema reports it missing
	}
	m := semver.FindStringSubmatch(v)
	if m == nil {
		return nil // the schema reports the pattern
	}
	if major, _ := strconv.Atoi(m[1]); major != SupportedMajor {
		return newErr(http.StatusConflict, CodeVersion, "ScreenJSON version %s is not supported; this server reads %d.x.x", v, SupportedMajor)
	}
	return nil
}

// fillIDs gives a UUID to every object that needs one and has none
// (R-DOC-01). An element's scene field is left as sent: a wrong one is a
// rule violation the checks report (SPEC.md 7.2), not something to hide.
func (m *Manager) fillIDs(doc map[string]any) {
	fill := func(o map[string]any) {
		if _, ok := o["id"].(string); !ok {
			o["id"] = m.ids.next()
		}
	}
	each := func(v any, f func(map[string]any)) {
		for _, item := range asList(v) {
			if o, ok := item.(map[string]any); ok {
				f(o)
			}
		}
	}
	fill(doc)
	for _, k := range []string{"authors", "contributors", "characters", "sources", "revisions"} {
		each(doc[k], fill)
	}
	wrapper, _ := doc["document"].(map[string]any)
	each(wrapper["bookmarks"], fill)
	each(wrapper["scenes"], func(s map[string]any) {
		fill(s)
		each(s["body"], func(el map[string]any) {
			fill(el)
			each(el["notes"], fill)
			each(el["revisions"], fill)
		})
	})
	if a, ok := doc["analysis"].(map[string]any); ok {
		each(a["passages"], fill)
		each(a["summaries"], fill)
	}
}

// prepare validates a whole document and makes its tree (SPEC.md 7.1, 7.2).
func (m *Manager) prepare(doc map[string]any) (*model.Tree, error) {
	if err := checkVersion(doc); err != nil {
		return nil, err
	}
	m.fillIDs(doc)
	if d := schema.Validate(m.o.Schema.Document(), doc); d != nil {
		return nil, invalid("the document does not match the schema", d)
	}
	t, err := model.FromMap(doc)
	if err != nil {
		return nil, invalidf("", "%v", err)
	}
	if d := checkDocument(t); d != nil {
		return nil, invalid("the document breaks a cross-reference rule (SPEC.md 7.2)", d)
	}
	// cast is the server's to keep (R-DER-01): a stale one is replaced.
	for _, s := range t.Scenes() {
		if cast, stale := staleCast(s); stale {
			s.Fields["cast"] = cast
		}
	}
	return t, nil
}

// ValidateImport checks a ScreenJSON document with the same schema and
// cross-reference rules used by Create, without writing it to storage.
func (m *Manager) ValidateImport(body []byte) error {
	doc, err := decodeObject(body)
	if err != nil {
		return err
	}
	_, err = m.prepare(doc)
	return err
}

// creating serialises imports of the same document ID.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyedMutex) lock(id string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	l, ok := k.locks[id]
	if !ok {
		l = &sync.Mutex{}
		k.locks[id] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Create answers POST /documents: import a whole document (R-DOC-01).
func (m *Manager) Create(ctx context.Context, req *Request) (*Response, error) {
	doc, err := decodeObject(req.Body)
	if err != nil {
		return nil, err
	}
	t, err := m.prepare(doc)
	if err != nil {
		return nil, err
	}
	id := t.Doc()
	unlock := m.creating.lock(id)
	defer unlock()

	if existing, err := m.Tree(ctx, id); err == nil {
		// R-API-14 for documents: an identical retry is 200, anything else a
		// conflict.
		a, _ := existing.JSON()
		b, _ := t.JSON()
		if jsonEqual(stripAnalysis(a, b), b) {
			return m.documentResponse(http.StatusOK, existing, req), nil
		}
		return nil, newErr(http.StatusConflict, CodeDuplicateID, "document %s already exists with different content", id)
	} else if e, ok := err.(*Error); !ok || (e.Status != http.StatusNotFound && e.Status != http.StatusGone) {
		return nil, err
	}

	now := m.o.Now()
	if err := layout.AssignOrderKeys(t); err != nil {
		return nil, err
	}
	stamp := func(n *model.Node) {
		n.Env.Rev, n.Env.TextRev, n.Env.CRev, n.Env.Updated = 1, 1, 1, now
	}
	_ = t.Root.Walk(func(n *model.Node) error { stamp(n); return nil })
	t.Each(stamp)
	t.Root.Env.Seq = 1
	t.Root.Env.Layout = m.o.Fingerprint

	save := func(ctx context.Context) error {
		puts, err := m.o.Layout.Records(t)
		if err != nil {
			return err
		}
		return layout.WritePuts(ctx, m.o.Driver, puts)
	}
	if m.o.Transactions {
		err = m.o.Driver.Tx(ctx, save)
	} else {
		err = save(ctx)
	}
	if err != nil {
		return nil, storageErr(err)
	}
	m.gone.remove(id)
	if err := m.o.State.PutDoc(IndexEntry(t)); err != nil {
		m.o.Log.Error("update document index", "doc", id, "err", err)
	}
	ev := Event{Seq: 1, Doc: id, Op: OpCreated, Kind: string(model.KindDocument), ID: id,
		Path: "/documents/" + id, Actor: req.Actor, TS: now}
	m.o.Publisher.Publish(id, []Event{ev})
	m.o.Hooks.Enqueue(HookEvent{Event: OpCreated, Doc: id, ID: id, Kind: string(model.KindDocument), Path: ev.Path})
	return m.documentResponse(http.StatusCreated, t, req), nil
}

// stripAnalysis drops analysis from a when b has none, so a retry compares
// equal whatever the stored document gained since.
func stripAnalysis(a, b map[string]any) map[string]any {
	if _, ok := b["analysis"]; ok {
		return a
	}
	out := make(map[string]any, len(a))
	for k, v := range a {
		if k != "analysis" {
			out[k] = v
		}
	}
	return out
}

func (m *Manager) documentResponse(status int, t *model.Tree, req *Request) *Response {
	v, _ := t.JSON()
	r := nodeResponse(status, t, t.Root, rootView(v, req))
	r.Location = "/documents/" + t.Doc()
	return r
}

// Replace answers PUT /documents/{doc}: the whole document is replaced
// (R-DOC-04). If-Match is compared to seq. Nodes whose IDs stay under the same
// parent keep their order keys, and their revisions unless they changed.
func (m *Manager) Replace(ctx context.Context, req *Request) (*Response, error) {
	doc, err := decodeObject(req.Body)
	if err != nil {
		return nil, err
	}
	docID := req.Target.Doc()
	if id, ok := doc["id"]; ok && id != docID {
		return nil, invalidf("/id", "the body's id %v is not the document's %s", id, docID)
	}
	doc["id"] = docID
	next, err := m.prepare(doc)
	if err != nil {
		return nil, err
	}
	return m.submit(ctx, docID, req, func(w *write) (replyFunc, error) {
		if err := m.precondition(req, w.cur.Seq(), false); err != nil {
			return nil, err
		}
		if err := m.co.check(docID, w.cur.Root, true, req.Token); err != nil {
			return nil, err
		}
		// Locked elements must come through unchanged, or merely unlocked.
		var lockErr error
		_ = w.cur.Root.Walk(func(old *model.Node) error {
			if lockErr != nil || old.Kind != model.KindElement || old.Fields["locked"] != true {
				return nil
			}
			n := next.Node(old.ID)
			if n == nil {
				lockErr = lockedErr(old)
				return nil
			}
			lockErr = checkLock(old, n)
			return nil
		})
		if lockErr != nil {
			return nil, lockErr
		}
		if err := w.replaceWith(next); err != nil {
			return nil, err
		}
		return func() *Response { return m.documentResponse(http.StatusOK, w.t, req) }, nil
	})
}

// replaceWith swaps the clone for a new tree, carrying envelopes over and
// scheduling every record: all of the new tree's, and deletes for records of
// nodes that are gone.
func (w *write) replaceWith(next *model.Tree) error {
	cur := w.cur
	carry := func(n *model.Node) {
		old := cur.Node(n.ID)
		if n.Kind == model.KindDocument {
			old = cur.Root
		} else if n.Kind == model.KindAnalysis {
			old = cur.Root.Analysis()
		}
		if old == nil {
			n.Env = model.Envelope{Rev: 1, TextRev: 1, CRev: 1, Updated: w.now}
			return
		}
		env := old.Env
		sameParent := (old.Parent == nil && n.Parent == nil) ||
			(old.Parent != nil && n.Parent != nil && old.Parent.ID == n.Parent.ID)
		if !sameParent {
			env.Order = ""
		}
		if !jsonEqual(old.Fields, n.Fields) || old.Type != n.Type {
			env.Rev++
			env.Updated = w.now
			if !jsonEqual(textSig(old), textSig(n)) {
				env.TextRev++
			}
		}
		n.Env = env
	}
	_ = next.Root.Walk(func(n *model.Node) error { carry(n); return nil })
	next.Each(func(n *model.Node) {
		if !isEmbedded(n) {
			return
		}
		carry(n)
	})
	lists := [][]*model.Node{next.Root.Characters(), next.Scenes()}
	parents := []*model.Node{next.Root, next.Root}
	for _, s := range next.Scenes() {
		lists = append(lists, s.Body())
		parents = append(parents, s)
	}
	for i, list := range lists {
		keys := make([]string, len(list))
		ok := true
		for j, n := range list {
			keys[j] = n.Env.Order
			if !order.Valid(keys[j]) || (j > 0 && keys[j-1] >= keys[j]) {
				ok = false
			}
		}
		if ok && !order.NeedsRekey(keys) {
			continue
		}
		spread, err := order.Spread(len(list))
		if err != nil {
			return err
		}
		for j, n := range list {
			n.Env.Order = spread[j]
		}
		parents[i].Env.CRev++
	}

	// Records of nodes that no longer exist.
	_ = cur.Root.Walk(func(old *model.Node) error {
		if old.Kind == model.KindDocument {
			return nil
		}
		if old.Kind == model.KindAnalysis {
			if next.Root.Analysis() == nil {
				w.removeRecord(old)
			}
			return nil
		}
		if next.Node(old.ID) == nil {
			w.removeRecord(old)
		}
		return nil
	})
	cur.Each(func(old *model.Node) {
		if next.Node(old.ID) == nil {
			w.deleted = append(w.deleted, old.ID)
		}
	})
	next.Root.Env.Seq = cur.Seq()
	w.t = next
	w.replacing = true
	return nil
}

// Delete a whole document (DELETE /documents/{doc}).
func (m *Manager) DeleteDocument(ctx context.Context, req *Request) (*Response, error) {
	docID := req.Target.Doc()
	return m.submit(ctx, docID, req, func(w *write) (replyFunc, error) {
		if err := m.precondition(req, w.cur.Seq(), false); err != nil {
			return nil, err
		}
		if err := m.co.check(docID, w.cur.Root, true, req.Token); err != nil {
			return nil, err
		}
		if err := m.o.Layout.Delete(ctx, m.o.Driver, docID); err != nil {
			return nil, storageErr(err)
		}
		w.deleting = true
		seq := w.cur.Seq() + 1
		w.afterCommit = append(w.afterCommit, func() {
			m.gone.add(docID)
			m.co.dropDoc(docID)
			if err := m.o.State.DeleteDoc(docID); err != nil {
				m.o.Log.Error("remove from document index", "doc", docID, "err", err)
			}
			m.forget(w.d)
			ev := Event{Seq: seq, Doc: docID, Op: OpDeleted, Kind: string(model.KindDocument), ID: docID,
				Path: "/documents/" + docID, Actor: req.Actor, TS: w.now}
			m.o.Publisher.Publish(docID, []Event{ev})
			m.o.Hooks.Enqueue(HookEvent{Event: OpDeleted, Doc: docID, ID: docID, Kind: string(model.KindDocument), Path: ev.Path})
		})
		return func() *Response { return &Response{Status: http.StatusNoContent, Seq: seq} }, nil
	})
}

// List answers GET /documents from the state-store index (SPEC.md 6.13).
func (m *Manager) List(req *Request) (*Response, error) {
	q := req.Query
	query := state.Query{Q: q.Get("q"), Genre: q.Get("genre"), Theme: q.Get("theme"), Lang: q.Get("lang"),
		Sort: q.Get("sort"), Order: q.Get("order"), Cursor: q.Get("cursor")}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, badRequest("offset must be a whole number, 0 or more")
		}
		query.Offset = n
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return nil, badRequest("limit must be a positive whole number")
		}
		query.Limit = n
	}
	page, err := m.o.State.Docs(query)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	items := make([]any, 0, len(page.Items))
	for _, e := range page.Items {
		items = append(items, e)
	}
	var next any
	if page.Next != "" {
		next = page.Next
	}
	body := map[string]any{"items": items, "next": next, "total": page.Total, "offset": page.Offset}
	if f := q.Get("facets"); f == "true" || f == "1" {
		body["facets"] = m.o.State.Facets()
	}
	return &Response{Status: http.StatusOK, Body: body}, nil
}

// Exists reports whether a document can be loaded.
func (m *Manager) Exists(ctx context.Context, id string) bool {
	_, err := m.Tree(ctx, id)
	return err == nil
}
