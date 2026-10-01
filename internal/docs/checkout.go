package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/state"
)

// Checkout is a time-limited claim on a node by one token (SPEC.md 6.8). They
// are held in memory only; a restart clears them (R-CO-05).
type Checkout struct {
	ID      string
	Doc     string
	NodeID  string
	Holder  string // the token's name
	Token   string // the token's hash
	Expires time.Time
	Pass    string
}

// checkouts holds every document's checkouts.
type checkouts struct {
	m     *Manager
	mu    sync.Mutex
	byDoc map[string]map[string]*Checkout
}

func newCheckouts(m *Manager) *checkouts {
	return &checkouts{m: m, byDoc: map[string]map[string]*Checkout{}}
}

func (c *checkouts) now() time.Time { return c.m.o.Now() }

// live returns a node's checkout if it hasn't expired. Callers hold c.mu.
func (c *checkouts) live(doc, node string) *Checkout {
	co := c.byDoc[doc][node]
	if co == nil || !co.Expires.After(c.now()) {
		return nil
	}
	return co
}

// has reports whether a document has any live checkout (it then stays cached).
func (c *checkouts) has(doc string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for node := range c.byDoc[doc] {
		if c.live(doc, node) != nil {
			return true
		}
	}
	return false
}

func (c *checkouts) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for doc, nodes := range c.byDoc {
		for node := range nodes {
			if c.live(doc, node) != nil {
				n++
			}
		}
	}
	return n
}

// countPass counts live checkouts taken for a pass.
func (c *checkouts) countPass(label string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for doc, nodes := range c.byDoc {
		for node, co := range nodes {
			if co.Pass == label && c.live(doc, node) != nil {
				n++
			}
		}
	}
	return n
}

// blocker finds a live checkout held by another token that covers a write to
// n (R-CO-02). A checkout on n or any ancestor covers every write; with
// structural set — a move, delete or replace — a checkout on any descendant
// covers it too. Callers hold c.mu. token "" (no token) is blocked by every
// checkout.
func (c *checkouts) blocker(doc string, n *model.Node, structural bool, token string) *Checkout {
	nodes := c.byDoc[doc]
	if len(nodes) == 0 {
		return nil
	}
	other := func(id string) *Checkout {
		if co := c.live(doc, id); co != nil && (token == "" || co.Token != token) {
			return co
		}
		return nil
	}
	for cur := n; cur != nil; cur = cur.Parent {
		if co := other(checkoutID(cur)); co != nil {
			return co
		}
	}
	if !structural {
		return nil
	}
	var found *Checkout
	_ = n.Walk(func(x *model.Node) error {
		if found == nil && x != n {
			found = other(checkoutID(x))
		}
		return nil
	})
	return found
}

// checkoutID is the key a node's checkout is held under.
func checkoutID(n *model.Node) string {
	if n.Kind == model.KindDocument {
		return n.Doc()
	}
	return n.ID
}

func checkedOut(co *Checkout) *Error {
	e := newErr(http.StatusLocked, CodeCheckedOut, "%s is checked out by %s", co.NodeID, co.Holder)
	e.Extra = map[string]any{"holder": co.Holder, "expires": co.Expires.UTC().Format(time.RFC3339Nano)}
	return e
}

// check refuses a write to n covered by another token's checkout.
func (c *checkouts) check(doc string, n *model.Node, structural bool, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if co := c.blocker(doc, n, structural, token); co != nil {
		return checkedOut(co)
	}
	return nil
}

// take checks a node out, or renews the caller's own checkout (R-CO-03). A
// node covered by another token's checkout — on itself, an ancestor or a
// descendant — can't be taken. Callers hold c.mu.
func (c *checkouts) take(doc string, n *model.Node, token, holder string, ttl time.Duration, pass string) (*Checkout, *Checkout) {
	if co := c.blocker(doc, n, true, token); co != nil {
		return nil, co
	}
	id := checkoutID(n)
	nodes := c.byDoc[doc]
	if nodes == nil {
		nodes = map[string]*Checkout{}
		c.byDoc[doc] = nodes
	}
	co := c.live(doc, id)
	if co == nil {
		co = &Checkout{ID: c.m.ids.next(), Doc: doc, NodeID: id, Holder: holder, Token: token}
		nodes[id] = co
	}
	co.Expires = c.now().Add(ttl)
	if pass != "" {
		co.Pass = pass
	}
	return co, nil
}

// release drops the caller's checkout on a node, reporting whether there was
// one.
func (c *checkouts) release(doc, node, token string) *Checkout {
	return c.releaseThen(doc, node, token, nil)
}

// releaseThen releases a token's checkout, running before first under the
// checkout lock when the token holds it.
func (c *checkouts) releaseThen(doc, node, token string, before func()) *Checkout {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.live(doc, node)
	if co == nil || co.Token != token {
		return nil
	}
	if before != nil {
		before()
	}
	delete(c.byDoc[doc], node)
	return co
}

// dropNodes forgets checkouts on deleted nodes.
func (c *checkouts) dropNodes(doc string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.byDoc[doc], id)
	}
}

// dropDoc forgets a deleted document's checkouts.
func (c *checkouts) dropDoc(doc string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byDoc, doc)
}

// list returns a document's live checkouts, soonest to expire first.
func (c *checkouts) list(doc string) []*Checkout {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Checkout
	for node := range c.byDoc[doc] {
		if co := c.live(doc, node); co != nil {
			cp := *co
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Expires.Before(out[j].Expires) })
	return out
}

// expire removes expired checkouts and announces each (R-CO-04).
func (c *checkouts) expire() {
	now := c.now()
	var gone []*Checkout
	c.mu.Lock()
	for doc, nodes := range c.byDoc {
		for id, co := range nodes {
			if !co.Expires.After(now) {
				gone = append(gone, co)
				delete(nodes, id)
			}
		}
		if len(nodes) == 0 {
			delete(c.byDoc, doc)
		}
	}
	c.mu.Unlock()
	for _, co := range gone {
		c.m.announce(co, true)
	}
}

// announce publishes checkout.changed. A released or expired checkout has no
// expiry.
func (m *Manager) announce(co *Checkout, ended bool) {
	ev := Event{Op: OpCheckoutChanged, Doc: co.Doc, NodeID: co.NodeID, Holder: co.Holder, TS: m.o.Now()}
	if !ended {
		exp := co.Expires
		ev.Expires = &exp
	}
	m.o.Publisher.Checkout(co.Doc, ev)
}

// passSet holds pass progress in memory over the state store, so a bulk
// checkout can filter finished nodes without a disk read each.
type passSet struct {
	st   *state.Store
	mu   sync.Mutex
	done map[string]map[string]bool
}

func newPassSet(st *state.Store) *passSet {
	return &passSet{st: st, done: map[string]map[string]bool{}}
}

// set returns a pass's finished nodes, loading them once.
func (p *passSet) set(label string) (map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.done[label]; ok {
		return s, nil
	}
	s, err := p.st.DoneSet(label)
	if err != nil {
		return nil, err
	}
	p.done[label] = s
	return s, nil
}

// markMemory records nodes as finished for a pass in memory, which is what
// bulk checkouts filter on. Call it before releasing the node's checkout, so
// no other worker can take a finished node (R-CO-09).
func (p *passSet) markMemory(label string, nodes ...string) error {
	if _, err := p.set(label); err != nil {
		return err
	}
	p.mu.Lock()
	for _, n := range nodes {
		p.done[label][n] = true
	}
	p.mu.Unlock()
	return nil
}

// persist writes finished nodes to the state store. It can wait for a batch
// and a disk sync, so it runs outside a document's write queue.
func (p *passSet) persist(label string, nodes ...string) error {
	return p.st.MarkDone(label, nodes...)
}

func (p *passSet) forget(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.done, label)
}

// checkoutBody is the request body of a checkout (SPEC.md 6.8, 6.9).
type checkoutBody struct {
	TTL     *float64       `json:"ttl"`
	Pass    string         `json:"pass"`
	Limit   int            `json:"limit"`
	Where   map[string]any `json:"where"`
	HasLang string         `json:"has_lang"`
	Include []string       `json:"include"`
}

func (m *Manager) ttl(b checkoutBody) (time.Duration, error) {
	if b.TTL == nil {
		return m.o.DefaultTTL, nil
	}
	d := time.Duration(*b.TTL * float64(time.Second))
	if d <= 0 {
		return 0, badRequest("ttl must be positive")
	}
	if d > m.o.MaxTTL {
		return 0, invalidf("/ttl", "ttl is at most %s", m.o.MaxTTL)
	}
	return d, nil
}

// checkoutJSON renders a checkout as SPEC.md 6.8 shows it.
func checkoutJSON(co *Checkout, n *model.Node, include []string) map[string]any {
	p, _ := paths.For(n)
	out := map[string]any{
		"id": co.ID, "path": p, "node_id": co.NodeID, "holder": co.Holder,
		"rev": n.Env.Rev, "text_rev": n.Env.TextRev,
		"expires": co.Expires.UTC().Format(time.RFC3339Nano), "pass": nil,
	}
	if co.Pass != "" {
		out["pass"] = co.Pass
	}
	if v, err := n.JSON(); err == nil {
		out["node"] = v
	}
	for _, inc := range include {
		if inc == "heading" && n.Kind == model.KindElement && n.Parent != nil {
			out["heading"] = n.Parent.Fields["heading"]
		}
	}
	return out
}

// checkoutable reports whether a kind of node can be checked out.
func checkoutable(n *model.Node) bool {
	switch n.Kind {
	case model.KindScene, model.KindElement, model.KindDocument, model.KindCharacter:
		return true
	}
	return false
}

// Checkout handles POST, PUT and DELETE on {node}/checkout.
func (m *Manager) Checkout(ctx context.Context, req *Request, method string) (*Response, error) {
	if req.Token == "" {
		return nil, newErr(http.StatusUnauthorized, CodeTokenRequired, "checkouts need a token (R-TOK-04)")
	}
	tg := req.Target
	t, n, err := m.resolveNode(ctx, tg)
	if err != nil {
		return nil, err
	}
	if !checkoutable(n) {
		return nil, badRequest("%s can't be checked out", n.Kind)
	}
	doc := t.Doc()
	switch method {
	case http.MethodDelete:
		id := checkoutID(n)
		label := req.Query.Get("pass")
		finish := label != "" && req.Query.Get("done") == "true"
		// Mark the node finished before its checkout goes, so no other
		// worker can take it in between.
		var markErr error
		co := m.co.releaseThen(doc, id, req.Token, func() {
			if finish {
				markErr = m.passes.markMemory(label, id)
			}
		})
		if co != nil {
			m.announce(co, true)
			if finish && markErr == nil {
				markErr = m.passes.persist(label, id)
			}
		}
		if markErr != nil {
			return nil, storageErr(markErr)
		}
		return &Response{Status: http.StatusNoContent, Seq: t.Seq()}, nil
	case http.MethodPost, http.MethodPut:
		var b checkoutBody
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &b); err != nil {
				return nil, badRequest("checkout body: %v", err)
			}
		}
		ttl, err := m.ttl(b)
		if err != nil {
			return nil, err
		}
		m.co.mu.Lock()
		if method == http.MethodPut {
			if co := m.co.live(doc, checkoutID(n)); co == nil || co.Token != req.Token {
				m.co.mu.Unlock()
				if co != nil {
					return nil, checkedOut(co)
				}
				return nil, notFound("you hold no checkout on %s to renew", checkoutID(n))
			}
		}
		co, blocker := m.co.take(doc, n, req.Token, req.Actor, ttl, b.Pass)
		var cp Checkout
		if co != nil {
			cp = *co
		}
		m.co.mu.Unlock()
		if blocker != nil {
			return nil, checkedOut(blocker)
		}
		if b.Pass != "" {
			if _, err := m.o.State.EnsurePass(b.Pass); err != nil {
				return nil, storageErr(err)
			}
		}
		m.announce(&cp, false)
		return &Response{Status: http.StatusOK, Body: checkoutJSON(&cp, n, b.Include), Seq: t.Seq()}, nil
	}
	return nil, badRequest("unsupported checkout method %s", method)
}

// Checkouts lists a document's active checkouts.
func (m *Manager) Checkouts(ctx context.Context, doc string) (*Response, error) {
	t, err := m.Tree(ctx, doc)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, co := range m.co.list(doc) {
		n := t.Node(co.NodeID)
		if co.NodeID == doc {
			n = t.Root
		}
		if n == nil {
			continue
		}
		j := checkoutJSON(co, n, nil)
		delete(j, "node")
		out = append(out, j)
	}
	return &Response{Status: http.StatusOK, Body: out, Seq: t.Seq()}, nil
}

// BulkCheckout checks out up to limit nodes in a scope (SPEC.md 6.9).
func (m *Manager) BulkCheckout(ctx context.Context, req *Request) (*Response, error) {
	if req.Token == "" {
		return nil, newErr(http.StatusUnauthorized, CodeTokenRequired, "checkouts need a token (R-TOK-04)")
	}
	var b checkoutBody
	if len(req.Body) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(req.Body)))
		dec.UseNumber()
		if err := dec.Decode(&b); err != nil {
			return nil, badRequest("checkout body: %v", err)
		}
	}
	ttl, err := m.ttl(b)
	if err != nil {
		return nil, err
	}
	if b.Limit <= 0 {
		b.Limit = 50
	}
	if b.Limit > 1000 {
		b.Limit = 1000
	}
	tg := req.Target
	var done map[string]bool
	var pass state.Pass
	if b.Pass != "" {
		if pass, err = m.o.State.EnsurePass(b.Pass); err != nil {
			return nil, storageErr(err)
		}
		if done, err = m.passes.set(b.Pass); err != nil {
			return nil, storageErr(err)
		}
	}

	docID := tg.Doc()
	library := docID == paths.Wildcard
	var docIDs []string
	cursor := ""
	if library {
		all, err := m.o.State.DocIDs()
		if err != nil {
			return nil, storageErr(err)
		}
		cursor = pass.Cursor
		i := sort.SearchStrings(all, cursor)
		if i < len(all) && all[i] == cursor {
			i++
		}
		docIDs = all[i:]
	} else {
		docIDs = []string{docID}
	}

	wantKind := model.KindElement
	if tg.Route.Kind == model.KindScene {
		wantKind = model.KindScene
	}
	wantType := model.ElementType(tg.Param(paths.ParamType))
	sceneID := tg.Param(paths.ParamScene)

	type claimed struct {
		co Checkout
		n  *model.Node
	}
	var got []claimed
	finishedPrefix := true
	newCursor := ""

	for _, id := range docIDs {
		if len(got) >= b.Limit {
			break
		}
		t, err := m.Tree(ctx, id)
		if err != nil {
			if e, ok := err.(*Error); ok && (e.Status == http.StatusNotFound || e.Status == http.StatusGone) {
				continue
			}
			return nil, err
		}
		var candidates []*model.Node
		for _, s := range t.Scenes() {
			if sceneID != "" && sceneID != paths.Wildcard && s.ID != sceneID {
				continue
			}
			if wantKind == model.KindScene {
				candidates = append(candidates, s)
				continue
			}
			for _, el := range s.Body() {
				if wantType == "" || el.Type == wantType {
					candidates = append(candidates, el)
				}
			}
		}

		unfinished := false
		m.co.mu.Lock()
		for _, n := range candidates {
			if done != nil && m.passDone(b.Pass, done, n.ID) {
				continue
			}
			if !matchWhere(n, b.Where) || !hasLang(n, b.HasLang) {
				continue
			}
			unfinished = true
			if len(got) >= b.Limit {
				break
			}
			co, blocker := m.co.take(id, n, req.Token, req.Actor, ttl, b.Pass)
			if blocker != nil {
				continue
			}
			got = append(got, claimed{co: *co, n: n})
		}
		m.co.mu.Unlock()
		if finishedPrefix && !unfinished {
			newCursor = id
		} else {
			finishedPrefix = false
		}
	}
	if library && b.Pass != "" && newCursor != "" && newCursor != cursor {
		if err := m.o.State.SetPassCursor(b.Pass, newCursor); err != nil {
			return nil, storageErr(err)
		}
	}

	out := make([]any, 0, len(got))
	for _, c := range got {
		m.announce(&c.co, false)
		out = append(out, checkoutJSON(&c.co, c.n, b.Include))
	}
	return &Response{Status: http.StatusOK, Body: map[string]any{"checkouts": out, "exhausted": len(got) == 0}}, nil
}

// passDone reads the done set under the pass lock.
func (m *Manager) passDone(label string, done map[string]bool, id string) bool {
	m.passes.mu.Lock()
	defer m.passes.mu.Unlock()
	return done[id]
}

// matchWhere applies a bulk checkout's where: equality on JSON Pointer fields
// of the node. A missing field matches false or null, so {"/locked": false}
// selects every element not locked, whether or not it carries the field.
func matchWhere(n *model.Node, where map[string]any) bool {
	for ptr, want := range where {
		got, ok := lookupPointer(n.Fields, ptr)
		if !ok {
			if want == false || want == nil {
				continue
			}
			return false
		}
		if !jsonEqual(got, want) {
			return false
		}
	}
	return true
}

// hasLang reports whether a node's text includes a language.
func hasLang(n *model.Node, lang string) bool {
	if lang == "" {
		return true
	}
	m, ok := textSig(n).(map[string]any)
	if !ok {
		return false
	}
	_, ok = m[lang]
	return ok
}

// lookupPointer reads a JSON Pointer inside a value.
func lookupPointer(v any, ptr string) (any, bool) {
	if ptr == "" {
		return v, true
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, false
	}
	cur := v
	for _, tok := range strings.Split(ptr[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch x := cur.(type) {
		case map[string]any:
			next, ok := x[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			var i int
			if _, err := fmt.Sscan(tok, &i); err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// jsonEqual compares JSON values, treating numbers by value.
func jsonEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	var va, vb any
	_ = json.Unmarshal(ja, &va)
	_ = json.Unmarshal(jb, &vb)
	return reflect.DeepEqual(va, vb)
}

// Passes lists every pass (R-CO-13).
func (m *Manager) Passes() ([]map[string]any, error) {
	ps, err := m.o.State.Passes()
	if err != nil {
		return nil, storageErr(err)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Label < ps[j].Label })
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, m.passJSON(p))
	}
	return out, nil
}

// Pass returns one pass.
func (m *Manager) Pass(label string) (map[string]any, error) {
	p, err := m.o.State.GetPass(label)
	if err == state.ErrNotFound {
		return nil, notFound("no pass %q", label)
	}
	if err != nil {
		return nil, storageErr(err)
	}
	return m.passJSON(p), nil
}

// DeletePass forgets a pass.
func (m *Manager) DeletePass(label string) error {
	if err := m.o.State.DeletePass(label); err == state.ErrNotFound {
		return notFound("no pass %q", label)
	} else if err != nil {
		return storageErr(err)
	}
	m.passes.forget(label)
	return nil
}

func (m *Manager) passJSON(p state.Pass) map[string]any {
	return map[string]any{
		"label": p.Label, "done": p.Done, "checked_out": m.co.countPass(p.Label),
		"created": p.Created.UTC().Format(time.RFC3339Nano), "updated": p.Updated.UTC().Format(time.RFC3339Nano),
	}
}

// PassStatus is what the pass:{label} channel carries.
func (m *Manager) PassStatus(label string) (done, checkedOut int) {
	p, err := m.o.State.GetPass(label)
	if err == nil {
		done = p.Done
	}
	return done, m.co.countPass(label)
}
