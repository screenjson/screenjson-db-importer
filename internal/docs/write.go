package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/layout"
	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
	"github.com/screenjson/screenjson-db-importer/internal/state"
)

// Request is one API call as the manager sees it.
type Request struct {
	Target *paths.Target
	// Actor is the token's name, or "anonymous" (R-TOK-03).
	Actor string
	// Token identifies the caller's token (its hash), or "" without one.
	Token string
	// IfMatch is the raw If-Match header.
	IfMatch string
	Query   url.Values
	Body    []byte
	// ContentType is the request's Content-Type, for routes that take files.
	ContentType string
	// AdminKey is the X-Admin-Key header, for PATCH /config.
	AdminKey string
}

// Response is what the API sends back.
type Response struct {
	Status int
	// Body is encoded as JSON. Nil means no body.
	Body any
	// Raw, when set, is sent as is with ContentType instead of Body (file
	// downloads).
	Raw         []byte
	ContentType string
	// ETag is the header value, quotes included, or "".
	ETag string
	// Seq is the document's seq after the request (ScreenJSON-Seq).
	Seq uint64
	// TextRev is set for nodes with text (ScreenJSON-Text-Rev).
	TextRev    uint64
	HasTextRev bool
	Location   string
	Header     map[string]string
}

// replyFunc renders the response once a write is committed, when revision
// numbers and seq are final.
type replyFunc func() *Response

// change records what happened to one tree node in a write.
type change struct {
	fields   bool // its own fields changed
	text     bool // its text changed, which bumps text_rev (worked out in commit)
	list     bool // its children were inserted, removed or reordered: crev
	inserted bool // it is new in this write
	moved    bool // its order or parent changed
	skipVal  bool // already validated by the operation (an embedding PUT)
}

// removal is a record to delete.
type removal struct {
	col, id string
	depth   int
}

// write is one operation's work on a clone of the document.
type write struct {
	m   *Manager
	d   *doc
	req *Request
	cur *model.Tree // committed
	t   *model.Tree // being built
	now time.Time

	changed  map[*model.Node]*change
	order    []*model.Node // tree nodes in the order first changed
	embedded map[string]*change

	removed   []removal
	events    []Event // structural events, in order
	deleted   []string
	undeleted []string
	hooks     []HookEvent

	bodyChanged   map[*model.Node]bool // scenes whose body changed: recompute cast
	scenesChanged bool                 // the scene list changed: renumber
	fullValidate  bool                 // validate the whole document (import, replace)
	deleting      bool                 // the document is being deleted
	noop          bool                 // nothing to store (an idempotent retry)
	replacing     bool                 // the whole document is being replaced
	afterCommit   []func()
	// later runs after the write leaves the queue but before its reply is
	// returned: slow work that needn't hold up the next write.
	later []func()
}

func (m *Manager) newWrite(d *doc, o *op) *write {
	cur := d.tree.Load()
	w := &write{
		m: m, d: d, req: o.req, cur: cur, now: m.o.Now(),
		changed: map[*model.Node]*change{}, embedded: map[string]*change{},
		bodyChanged: map[*model.Node]bool{},
	}
	if cur != nil {
		w.t = cur.Clone()
	}
	return w
}

// touch records a change to a tree node.
func (w *write) touch(n *model.Node) *change {
	c, ok := w.changed[n]
	if !ok {
		c = &change{}
		w.changed[n] = c
		w.order = append(w.order, n)
	}
	return c
}

// edit makes a tree node's Fields safe to change and records the change.
func (w *write) edit(n *model.Node) *model.Node {
	w.t.Own(n)
	w.touch(n).fields = true
	return n
}

func (w *write) touchEmbedded(id string) *change {
	c, ok := w.embedded[id]
	if !ok {
		c = &change{}
		w.embedded[id] = c
	}
	return c
}

// event adds a structural event.
func (w *write) event(ev Event) { w.events = append(w.events, ev) }

// markList records that a node's children changed.
func (w *write) markList(parent *model.Node) {
	w.touch(parent).list = true
	if parent.Kind == model.KindScene {
		w.bodyChanged[parent] = true
	}
	if parent.Kind == model.KindDocument {
		w.scenesChanged = true
	}
}

// removeRecord schedules a split node's record for deletion. Embedded nodes
// have no record of their own; their owner is rewritten instead.
func (w *write) removeRecord(n *model.Node) {
	l := w.m.o.Layout
	if n.Kind == model.KindDocument || !l.Splits(n.Kind) {
		return
	}
	w.removed = append(w.removed, removal{col: l.Collection(n.Kind), id: layout.RecordID(n), depth: depth(n)})
}

func depth(n *model.Node) int {
	d := 0
	for cur := n.Parent; cur != nil; cur = cur.Parent {
		d++
	}
	return d
}

// commit derives, validates and stores the write (SPEC.md 4.4 steps 2 to 5).
// On any failure the committed tree is untouched (R-ARCH-03).
func (w *write) commit(ctx context.Context) error {
	if w.deleting || w.noop {
		return nil
	}
	if w.replacing {
		return w.commitReplace(ctx)
	}
	if len(w.changed) == 0 && len(w.embedded) == 0 && len(w.events) == 0 && len(w.removed) == 0 {
		w.noop = true
		return nil
	}
	if err := w.t.Reindex(); err != nil {
		return invalidf("", "%v", err)
	}
	if err := w.derive(); err != nil {
		return err
	}
	if err := w.t.Reindex(); err != nil {
		return invalidf("", "%v", err)
	}
	if err := w.validate(); err != nil {
		return err
	}
	w.bump()
	return w.store(ctx)
}

// derive applies the rules of SPEC.md 7.4 and 7.5: text changes and their
// consequences, encryption and locks, cast and scene numbers.
func (w *write) derive() error {
	enc := w.encrypted()
	for _, n := range append([]*model.Node(nil), w.order...) {
		c := w.changed[n]
		if c.inserted || !c.fields {
			continue
		}
		old := w.oldNode(n)
		if old == nil {
			continue
		}
		if err := checkLock(old, n); err != nil {
			return err
		}
		if !reflect.DeepEqual(textSig(old), textSig(n)) {
			c.text = true
			if enc || ownEncrypted(n) {
				if !reflect.DeepEqual(encryptedText(old.Fields), encryptedText(n.Fields)) {
					return newErr(http.StatusConflict, CodeEncrypted,
						"%s is encrypted, so its text can't be changed through the server (R-ENC-02)", n)
				}
			}
			if err := w.textChanged(n); err != nil {
				return err
			}
		} else if (enc || ownEncrypted(n)) && !reflect.DeepEqual(encryptedText(old.Fields), encryptedText(n.Fields)) {
			return newErr(http.StatusConflict, CodeEncrypted, "%s is encrypted, so its text can't be changed through the server (R-ENC-02)", n)
		}
	}
	for id, c := range w.embedded {
		if c.inserted {
			continue
		}
		old, cur := w.cur.Node(id), w.t.Node(id)
		if old == nil || cur == nil {
			continue
		}
		if !reflect.DeepEqual(old.Fields["text"], cur.Fields["text"]) {
			c.text = true
			if enc && !reflect.DeepEqual(encryptedText(old.Fields), encryptedText(cur.Fields)) {
				return newErr(http.StatusConflict, CodeEncrypted, "%s is encrypted, so its text can't be changed through the server (R-ENC-02)", id)
			}
		}
	}

	// Cast follows the body of every scene whose elements changed (R-DER-01).
	for _, n := range w.order {
		if n.Kind == model.KindElement && n.Parent != nil {
			w.bodyChanged[n.Parent] = true
		}
	}
	scenes := make([]*model.Node, 0, len(w.bodyChanged))
	for s := range w.bodyChanged {
		scenes = append(scenes, s)
	}
	sort.Slice(scenes, func(i, j int) bool { return scenes[i].ID < scenes[j].ID })
	for _, s := range scenes {
		if s.Parent == nil { // removed in this write
			continue
		}
		w.updateCast(s)
	}
	if w.scenesChanged {
		w.renumber()
	}
	return nil
}

// oldNode finds the committed version of a node of the clone.
func (w *write) oldNode(n *model.Node) *model.Node {
	switch n.Kind {
	case model.KindDocument:
		return w.cur.Root
	case model.KindAnalysis:
		return w.cur.Root.Analysis()
	}
	return w.cur.Node(n.ID)
}

// encrypted reports whether the document is encrypted at the root (R-ENC-01).
func (w *write) encrypted() bool {
	v, ok := w.t.Root.Fields["encrypt"]
	return ok && v != nil
}

// ownEncrypted reports whether a node carries its own encrypt settings.
func ownEncrypted(n *model.Node) bool {
	v, ok := n.Fields["encrypt"]
	return ok && v != nil
}

// textSig is what a node's text_rev follows: the text its embeddings are made
// from (SPEC.md 6.11: heading for scenes, name for characters, text otherwise).
func textSig(n *model.Node) any {
	switch n.Kind {
	case model.KindScene:
		return n.Fields["heading"]
	case model.KindCharacter:
		return n.Fields["name"]
	case model.KindElement:
		if n.Type == model.TypeCharacter {
			return n.Fields["display"]
		}
		return n.Fields["text"]
	}
	return n.Fields["text"]
}

// encryptedText collects every value encryption covers (SPEC.md 3.4): text
// maps, note text, slugline desc, character desc, logline, cover extra.
func encryptedText(fields map[string]any) any {
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				switch k {
				case "text", "desc", "logline", "extra":
					if _, isMap := sub.(map[string]any); isMap {
						out[prefix+"/"+k] = sub
						continue
					}
				}
				walk(prefix+"/"+k, sub)
			}
		case []any:
			for i, sub := range x {
				walk(fmt.Sprintf("%s/%d", prefix, i), sub)
			}
		}
	}
	walk("", fields)
	return out
}

// checkLock enforces R-LOCK-01: a locked element takes no change except
// unlocking it.
func checkLock(old, cur *model.Node) error {
	if old.Kind != model.KindElement || old.Fields["locked"] != true {
		return nil
	}
	unlocked := make(map[string]any, len(old.Fields))
	for k, v := range old.Fields {
		unlocked[k] = v
	}
	unlocked["locked"] = false
	if reflect.DeepEqual(unlocked, cur.Fields) && old.Type == cur.Type {
		return nil
	}
	return lockedErr(old)
}

func lockedErr(n *model.Node) *Error {
	return newErr(http.StatusConflict, CodeLockedNode, "%s is locked; the only change allowed is {\"locked\": false}", n)
}

// lockedWithin reports the first locked element at or under n, for structural
// writes that would change it.
func lockedWithin(n *model.Node) *model.Node {
	var found *model.Node
	_ = n.Walk(func(x *model.Node) error {
		if found == nil && x.Kind == model.KindElement && x.Fields["locked"] == true {
			found = x
		}
		return nil
	})
	return found
}

// textChanged applies R-DER-02 and R-EMB-04: the node's embeddings go, and so
// do passages that include it and the summary of its scene.
func (w *write) textChanged(n *model.Node) error {
	w.dropEmbeddings(n.ID)
	w.dropPassages(func(p map[string]any) bool { return passageIncludes(p, n.ID) })
	scene := n
	if n.Kind == model.KindElement {
		scene = n.Parent
	}
	if scene != nil && scene.Kind == model.KindScene {
		w.dropSummaries(func(s map[string]any) bool { return s["scope"] == "scene" && s["target"] == scene.ID })
	}
	if n.Kind == model.KindElement || n.Kind == model.KindScene || n.Kind == model.KindCharacter {
		w.queueTextHook(n)
	}
	return nil
}

// analysis returns the clone's analysis node, made editable, creating it when
// create is set.
func (w *write) analysis(create bool) *model.Node {
	a := w.t.Root.Analysis()
	if a == nil {
		if !create {
			return nil
		}
		a = model.NewNode(model.KindAnalysis)
		w.t.Owns(a)
		w.t.Root.SetChildren("analysis", []*model.Node{a})
		c := w.touch(a)
		c.inserted, c.fields = true, true
		w.touch(w.t.Root)
		return a
	}
	return w.edit(a)
}

// dropEmbeddings removes every embedding of a node.
func (w *write) dropEmbeddings(id string) int {
	a := w.t.Root.Analysis()
	if a == nil {
		return 0
	}
	emb, _ := a.Fields["embeddings"].(map[string]any)
	list, _ := emb[id].([]any)
	if len(list) == 0 {
		return 0
	}
	a = w.edit(a)
	emb = a.Fields["embeddings"].(map[string]any)
	delete(emb, id)
	w.touchVectorOwner(id)
	return len(list)
}

// touchVectorOwner schedules the record of the node an embedding belongs to,
// since a natively stored vector lives in that record rather than in analysis
// (SPEC.md 5.5). The node itself hasn't changed, so its rev doesn't move.
func (w *write) touchVectorOwner(id string) {
	if n := w.t.Node(id); n != nil && n.Parent != nil && w.m.o.Layout.VectorFor(n.Kind) != nil {
		w.touch(n)
	}
}

// dropPassages removes passages matching a rule and reports how many.
func (w *write) dropPassages(match func(map[string]any) bool) int {
	return w.dropAnalysisItems("passages", match)
}

// dropSummaries removes summaries matching a rule and reports how many.
func (w *write) dropSummaries(match func(map[string]any) bool) int {
	return w.dropAnalysisItems("summaries", match)
}

func (w *write) dropAnalysisItems(field string, match func(map[string]any) bool) int {
	a := w.t.Root.Analysis()
	if a == nil {
		return 0
	}
	list, _ := a.Fields[field].([]any)
	hit := false
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && match(m) {
			hit = true
			break
		}
	}
	if !hit {
		return 0
	}
	a = w.edit(a)
	list, _ = a.Fields[field].([]any)
	kept := make([]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && match(m) {
			if id, _ := m["id"].(string); id != "" {
				w.deleted = append(w.deleted, id)
			}
			continue
		}
		kept = append(kept, item)
	}
	a.Fields[field] = kept
	return len(list) - len(kept)
}

func passageIncludes(p map[string]any, id string) bool {
	if p["scene"] == id {
		return true
	}
	els, _ := p["elements"].([]any)
	for _, e := range els {
		if e == id {
			return true
		}
	}
	return false
}

// updateCast sets a scene's cast from its body (R-DER-01).
func (w *write) updateCast(s *model.Node) {
	if cast, stale := staleCast(s); stale {
		w.edit(s).Fields["cast"] = cast
	}
}

// staleCast works out a scene's cast — the character IDs of cues and dialogue
// in order of first appearance — and reports whether the stored one differs. A
// scene with no cast field and no characters counts as up to date, so
// documents that never had the field round-trip unchanged.
func staleCast(s *model.Node) ([]any, bool) {
	cast := []any{}
	seen := map[string]bool{}
	for _, el := range s.Body() {
		if el.Type != model.TypeCharacter && el.Type != model.TypeDialogue {
			continue
		}
		id, _ := el.Fields["character"].(string)
		if id != "" && !seen[id] {
			seen[id] = true
			cast = append(cast, id)
		}
	}
	old, present := s.Fields["cast"]
	if !present && len(cast) == 0 {
		return nil, false
	}
	if present {
		if list, ok := old.([]any); ok && reflect.DeepEqual(list, cast) {
			return nil, false
		}
	}
	return cast, true
}

// numberingMode reads the document's numbering mode (R-DER-03).
func (w *write) numberingMode() string {
	if wrapper, ok := w.t.Root.Fields["document"].(map[string]any); ok {
		if meta, ok := wrapper["meta"].(map[string]any); ok {
			if v, ok := meta["screenjson-server.numbering"].(string); ok && v != "" {
				return v
			}
		}
	}
	return w.m.o.Numbering
}

// renumber applies the numbering mode after the scene list changed.
func (w *write) renumber() {
	switch w.numberingMode() {
	case "auto":
		for i, s := range w.t.Scenes() {
			h, _ := s.Fields["heading"].(map[string]any)
			if no, ok := h["no"]; ok && fmt.Sprint(no) == fmt.Sprint(i+1) {
				continue
			}
			s = w.edit(s)
			h, _ = s.Fields["heading"].(map[string]any)
			if h == nil {
				continue
			}
			h["no"] = json.Number(fmt.Sprint(i + 1))
		}
	case "frozen":
		for _, s := range w.t.Scenes() {
			if c := w.changed[s]; c == nil || !c.inserted {
				continue
			}
			if h, ok := s.Fields["heading"].(map[string]any); ok {
				delete(h, "no")
			}
		}
	}
}

// validate checks every changed node as its kind (R-VAL-03) and the whole
// document against the rules of SPEC.md 7.2 (R-VAL-05).
func (w *write) validate() error {
	set := w.m.o.Schema
	if w.fullValidate {
		v, err := w.t.JSON()
		if err != nil {
			return invalidf("", "%v", err)
		}
		if d := schema.Validate(set.Document(), v); d != nil {
			return invalid("the document does not match the schema", d)
		}
	} else {
		for _, n := range w.order {
			c := w.changed[n]
			if c.skipVal || n.Parent == nil && n.Kind != model.KindDocument {
				continue
			}
			if d := validateNode(set, n); d != nil {
				return invalid(fmt.Sprintf("%s does not match the schema", n), d)
			}
		}
	}
	// A write that only edits scenes or elements in place can only break the
	// references those nodes hold, so only they are checked; if they fail, the
	// full check runs to report the same pointers it always would.
	if w.local() && checkNodes(w.t, w.order) {
		return nil
	}
	if d := checkDocument(w.t); d != nil {
		return invalid("the document breaks a cross-reference rule (SPEC.md 7.2)", d)
	}
	return nil
}

// local reports whether the write changed scenes or elements in place and
// nothing else: no inserts, moves, deletions or root and character changes,
// so the document-wide rules of SPEC.md 7.2 (bookmarks, passages, summaries,
// the cover, at least one scene) can't have changed.
func (w *write) local() bool {
	if w.fullValidate || w.replacing || w.scenesChanged || len(w.events) > 0 || len(w.removed) > 0 ||
		len(w.deleted) > 0 || len(w.undeleted) > 0 || len(w.embedded) > 0 {
		return false
	}
	for _, n := range w.order {
		if n.Kind != model.KindElement && n.Kind != model.KindScene {
			return false
		}
	}
	return len(w.order) > 0
}

// validateNode checks one tree node with the validator for its kind.
func validateNode(set *schema.Set, n *model.Node) []schema.Defect {
	switch n.Kind {
	case model.KindDocument, model.KindScene:
		v, err := n.JSONExcept(func(k model.Kind) bool {
			return k == model.KindScene || k == model.KindElement || k == model.KindCharacter || k == model.KindAnalysis
		})
		if err != nil {
			return []schema.Defect{{Message: err.Error()}}
		}
		s, _ := set.Shallow(string(n.Kind))
		return schema.Validate(s, v)
	case model.KindElement:
		s, ok := set.Element(string(n.Type))
		if !ok {
			return []schema.Defect{{Pointer: "/type", Message: fmt.Sprintf("unknown element type %q", n.Type)}}
		}
		return schema.Validate(s, n.Fields)
	default:
		s, ok := set.Kind(string(n.Kind))
		if !ok {
			return nil
		}
		v, err := n.JSON()
		if err != nil {
			return []schema.Defect{{Message: err.Error()}}
		}
		return schema.Validate(s, v)
	}
}

// bump moves revision numbers and seq on (SPEC.md 2). Events get their seq
// here, one each (R-EVT-06).
func (w *write) bump() {
	for _, n := range w.order {
		c := w.changed[n]
		n.Env.Updated = w.now
		if c.inserted {
			n.Env.Rev, n.Env.TextRev, n.Env.CRev = 1, 1, 1
			continue
		}
		if c.fields || c.moved {
			n.Env.Rev++
		}
		if c.text {
			n.Env.TextRev++
		}
		if c.list {
			n.Env.CRev++
		}
	}
	for id, c := range w.embedded {
		n := w.t.Node(id)
		if n == nil {
			continue
		}
		n.Env.Updated = w.now
		if c.inserted {
			n.Env.Rev, n.Env.TextRev, n.Env.CRev = 1, 1, 1
			continue
		}
		n.Env.Rev++
		if c.text {
			n.Env.TextRev++
		}
	}

	// node.updated for every changed node not already covered by a structural
	// event about it.
	covered := map[*model.Node]bool{}
	for _, ev := range w.events {
		if ev.Op == OpInserted || ev.Op == OpRemoved {
			if n := w.eventNode(ev); n != nil {
				covered[n] = true
			}
		}
	}
	var updates []Event
	for _, n := range w.order {
		c := w.changed[n]
		if c.inserted || !c.fields || covered[n] || n.Parent == nil && n.Kind != model.KindDocument {
			continue
		}
		updates = append(updates, w.nodeEvent(OpUpdated, n))
	}
	w.events = append(w.events, updates...)
	seq := w.cur.Seq()
	for i := range w.events {
		seq++
		w.events[i].Seq = seq
		w.events[i].Doc = w.t.Doc()
		w.events[i].Actor = w.req.Actor
		w.events[i].TS = w.now
		w.fillEventNode(&w.events[i])
	}
	if len(w.events) == 0 {
		seq++
	}
	w.t.Root.Env.Seq = seq
	w.t.Root.Env.Layout = w.m.o.Fingerprint
}

// eventNode finds the node an event is about in the clone.
func (w *write) eventNode(ev Event) *model.Node {
	switch ev.Kind {
	case string(model.KindDocument):
		return w.t.Root
	case string(model.KindAnalysis):
		return w.t.Root.Analysis()
	}
	return w.t.Node(ev.ID)
}

// nodeEvent builds an event about a tree node; its node body, rev and seq are
// filled in by bump.
func (w *write) nodeEvent(op string, n *model.Node) Event {
	ev := Event{Op: op, Kind: string(n.Kind), Type: string(n.Type), ID: n.ID}
	if n.Kind == model.KindAnalysis {
		ev.ID = w.t.Doc()
	}
	if p, err := paths.For(n); err == nil {
		ev.Path = p
	}
	if n.Parent != nil {
		ev.Parent = layout.RecordID(n.Parent)
	}
	return ev
}

// fillEventNode adds the node body and revision numbers to node events, now
// that they are final.
func (w *write) fillEventNode(ev *Event) {
	if ev.Op != OpUpdated && ev.Op != OpInserted && ev.Op != OpMoved {
		return
	}
	n := w.eventNode(*ev)
	if n == nil {
		return
	}
	ev.Rev, ev.TextRev, ev.Order = n.Env.Rev, n.Env.TextRev, n.Env.Order
	if ev.Op == OpMoved {
		return
	}
	ev.Node = eventBody(n, ev.Op == OpInserted)
}

// eventBody is the ScreenJSON an event carries (see Event).
func eventBody(n *model.Node, inserted bool) json.RawMessage {
	split := func(k model.Kind) bool {
		if inserted && n.Kind == model.KindScene && k == model.KindElement {
			return false
		}
		return k == model.KindScene || k == model.KindElement || k == model.KindCharacter || k == model.KindAnalysis
	}
	v, err := n.JSONExcept(split)
	if err != nil {
		return nil
	}
	if n.Kind == model.KindAnalysis {
		delete(v, "embeddings")
	}
	return marshalNode(v)
}

// store writes the records the change needs (SPEC.md 4.4 step 5).
//
// Commit write rule (R-ARCH-05): deletes go first, deepest records first, so a
// crash leaves at worst orphans, which loading ignores and fsck removes — a
// deleted scene disappears with its record, before its elements. Then writes go
// deepest first and the root last: an inserted scene's elements are written
// before the scene that makes them reachable, a moved element's own record
// (its new parent and order key) is written before the scenes whose cast it
// changes, and the root, which carries seq, comes last.
func (w *write) store(ctx context.Context) error {
	l := w.m.o.Layout
	owners := map[*model.Node]bool{w.t.Root: true}
	for _, n := range w.order {
		if n.Parent == nil && n.Kind != model.KindDocument {
			continue // removed in this write
		}
		if o := l.Owner(n); o != nil {
			owners[o] = true
		}
	}
	for id := range w.embedded {
		if n := w.t.Node(id); n != nil {
			if o := l.Owner(n); o != nil {
				owners[o] = true
			}
		}
	}
	nodes := make([]*model.Node, 0, len(owners))
	for n := range owners {
		nodes = append(nodes, n)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		di, dj := depth(nodes[i]), depth(nodes[j])
		if di != dj {
			return di > dj
		}
		return layout.RecordID(nodes[i]) < layout.RecordID(nodes[j])
	})
	puts, err := l.RecordsOf(w.t, nodes)
	if err != nil {
		return storageErr(err)
	}
	sort.SliceStable(w.removed, func(i, j int) bool { return w.removed[i].depth < w.removed[j].depth })

	d := w.m.o.Driver
	run := func(ctx context.Context) error {
		for _, r := range w.removed {
			if err := d.Delete(ctx, r.col, []string{r.id}); err != nil {
				return fmt.Errorf("delete %s from %s: %w", r.id, r.col, err)
			}
		}
		return layout.WritePuts(ctx, d, puts)
	}
	if w.m.o.Transactions && (len(puts)+len(w.removed) > 1) {
		err = d.Tx(ctx, run)
	} else {
		err = run(ctx)
	}
	if err != nil {
		return storageErr(err)
	}
	return nil
}

// commitReplace stores a whole-document replacement: every record of the new
// tree, after deleting the records of nodes that are gone, with one
// document.replaced event (SPEC.md 8.3).
func (w *write) commitReplace(ctx context.Context) error {
	seq := w.cur.Seq() + 1
	w.t.Root.Env.Seq = seq
	w.t.Root.Env.Layout = w.m.o.Fingerprint
	w.t.Root.Env.Updated = w.now
	w.events = []Event{{Seq: seq, Doc: w.t.Doc(), Op: OpReplaced, Kind: string(model.KindDocument),
		ID: w.t.Doc(), Path: "/documents/" + w.t.Doc(), Actor: w.req.Actor, TS: w.now}}
	puts, err := w.m.o.Layout.Records(w.t)
	if err != nil {
		return storageErr(err)
	}
	d := w.m.o.Driver
	run := func(ctx context.Context) error {
		for _, r := range w.removed {
			if err := d.Delete(ctx, r.col, []string{r.id}); err != nil {
				return err
			}
		}
		return layout.WritePuts(ctx, d, puts)
	}
	if w.m.o.Transactions {
		err = d.Tx(ctx, run)
	} else {
		err = run(ctx)
	}
	if err != nil {
		return storageErr(err)
	}
	return nil
}

// finish makes a committed write visible (SPEC.md 4.4 steps 6 to 8): the new
// tree replaces the old, the index is updated, and events and webhooks go out.
func (w *write) finish() {
	if w.noop {
		return
	}
	if w.deleting {
		for _, f := range w.afterCommit {
			f()
		}
		return
	}
	w.t.MarkDeleted(w.deleted...)
	w.t.Undelete(w.undeleted...)
	w.d.tree.Store(w.t)
	w.d.lastWrite.Store(w.now.UnixNano())
	if len(w.deleted) > 0 {
		w.m.co.dropNodes(w.t.Doc(), w.deleted)
	}
	// Built when read or flushed, not on every write: it walks the document.
	t := w.t
	w.m.o.State.PutDocLazy(t.Doc(), func() state.DocEntry { return IndexEntry(t) })
	for _, f := range w.afterCommit {
		f()
	}
	if len(w.events) > 0 {
		w.m.o.Publisher.Publish(w.t.Doc(), w.events)
	}
	w.sendHooks()
}

// Seq is the document's seq once committed, for responses.
func (w *write) Seq() uint64 { return w.t.Root.Env.Seq }
