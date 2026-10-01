package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Tree is one cached document: its node graph plus the indexes the API needs to
// answer questions about it quickly.
type Tree struct {
	// Root is the document node.
	Root *Node
	// index maps a UUID to the node it names, so /nodes/{uuid} and stale-path
	// redirects are one lookup (SPEC.md R-PATH-06).
	index map[string]*Node
	// deleted remembers recently deleted IDs so a request for one can answer 410
	// rather than 404 (R-PATH-08). It is shared by every clone of a document's
	// tree, since it records history rather than content, and is only changed
	// once a write has been committed.
	deleted *deletedSet
	// owned records the nodes whose Fields this tree has copied, so Own copies
	// each at most once. Nil on a tree that isn't a clone being written.
	owned map[*Node]struct{}
	// oldIndex is the index being replaced, during a reindex only.
	oldIndex map[string]*Node
}

// deletedSet is a bounded, ordered set of deleted IDs, safe for concurrent use.
type deletedSet struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
}

func newDeletedSet() *deletedSet { return &deletedSet{ids: map[string]struct{}{}} }

func (d *deletedSet) has(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.ids[id]
	return ok
}

func (d *deletedSet) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.ids)
}

// MaxDeletedTracked is how many deleted IDs a document remembers. Past this the
// oldest is forgotten and a request for it answers 404 instead of 410
// (SPEC.md 6.5).
const MaxDeletedTracked = 10000

// Parse builds a Tree from a whole ScreenJSON document.
//
// It takes the document apart along the levels a layout may split
// (SPEC.md 5.2): scenes out of document.scenes, elements out of each scene's
// body, characters out of the root, and analysis out of the root. Everything
// else stays embedded in its parent's Fields. Reassembling with Tree.JSON gives
// back a byte-identical document, which is what the layout round trips of
// R-VER-02 check.
//
// Numbers are decoded as json.Number so that a scene number or a vector
// component keeps its exact spelling through a round trip.
func Parse(raw []byte) (*Tree, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("model: parse document: %w", err)
	}
	return FromMap(doc)
}

// FromMap builds a Tree from an already-decoded document.
func FromMap(doc map[string]any) (*Tree, error) {
	root, err := buildNode(doc, KindDocument, nil)
	if err != nil {
		return nil, err
	}
	if id, ok := root.Fields["id"].(string); ok {
		root.ID = id
	}

	t := &Tree{
		Root:    root,
		index:   map[string]*Node{},
		deleted: newDeletedSet(),
	}
	if err := t.Reindex(); err != nil {
		return nil, err
	}
	return t, nil
}

// Build turns a decoded object into a detached node of a kind, taking its
// splittable children apart as FromMap does: a scene's body becomes element
// nodes. Its Fields is obj itself, so the caller must not share obj.
func Build(kind Kind, obj map[string]any) (*Node, error) {
	return buildNode(obj, kind, nil)
}

// buildNode turns a decoded object into a node, pulling out the children the
// layout may split.
func buildNode(obj map[string]any, kind Kind, parent *Node) (*Node, error) {
	n := &Node{
		Kind:   kind,
		Parent: parent,
		Fields: obj,
		Kids:   map[string][]*Node{},
	}

	if kind == KindElement {
		typ, ok := obj["type"].(string)
		if !ok {
			return nil, fmt.Errorf("model: element has no type field")
		}
		t, err := ParseElementType(typ)
		if err != nil {
			return nil, err
		}
		n.Type = t
	}

	if kind != KindAnalysis {
		if id, ok := obj["id"].(string); ok {
			n.ID = id
		}
	}

	for _, field := range childFields[kind] {
		raw, present := takePath(obj, field.path)
		if !present {
			continue
		}
		if field.single {
			asMap, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("model: %s is not an object", field.name)
			}
			kid, err := buildNode(asMap, field.kind, n)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", field.name, err)
			}
			n.Kids[field.name] = []*Node{kid}
			continue
		}

		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("model: %s is not an array", field.name)
		}
		kids := make([]*Node, 0, len(items))
		for i, item := range items {
			asMap, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("model: %s[%d] is not an object", field.name, i)
			}
			kid, err := buildNode(asMap, field.kind, n)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", field.name, i, err)
			}
			kids = append(kids, kid)
		}
		n.Kids[field.name] = kids
	}

	return n, nil
}

// JSON reassembles the whole document.
func (t *Tree) JSON() (map[string]any, error) {
	return t.Root.JSON()
}

// Marshal encodes the document as ordinary JSON.
func (t *Tree) Marshal() ([]byte, error) {
	v, err := t.JSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Doc returns the document's UUID.
func (t *Tree) Doc() string { return t.Root.ID }

// Seq returns the document's sequence number.
func (t *Tree) Seq() uint64 { return t.Root.Env.Seq }

// Node returns the node a UUID names, or nil.
func (t *Tree) Node(id string) *Node { return t.index[id] }

// Has reports whether a UUID is live in this document.
func (t *Tree) Has(id string) bool {
	_, ok := t.index[id]
	return ok
}

// WasDeleted reports whether a UUID was deleted recently enough to still be
// remembered, which is the difference between 410 and 404 (R-PATH-08).
func (t *Tree) WasDeleted(id string) bool { return t.deleted.has(id) }

// MarkDeleted removes IDs from the index and remembers them as deleted,
// forgetting the oldest past the limit. Call it only once the delete has been
// committed, since the record is shared with the tree readers still see.
func (t *Tree) MarkDeleted(ids ...string) {
	d := t.deleted
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		delete(t.index, id)
		if _, already := d.ids[id]; already {
			continue
		}
		d.ids[id] = struct{}{}
		d.order = append(d.order, id)
	}
	for len(d.order) > MaxDeletedTracked {
		delete(d.ids, d.order[0])
		d.order = d.order[1:]
	}
}

// Undelete forgets that IDs were deleted: a client re-inserted a node under an
// ID it had deleted, so it is live again. Like MarkDeleted, call it after the
// insert is committed.
func (t *Tree) Undelete(ids ...string) {
	d := t.deleted
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for _, id := range ids {
		if _, ok := d.ids[id]; ok {
			delete(d.ids, id)
			changed = true
		}
	}
	if !changed {
		return
	}
	kept := d.order[:0]
	for _, id := range d.order {
		if _, ok := d.ids[id]; ok {
			kept = append(kept, id)
		}
	}
	d.order = kept
}

// Index adds a node to the UUID index.
func (t *Tree) Index(n *Node) {
	if n.ID == "" || !n.Kind.HasUUID() {
		return
	}
	t.index[n.ID] = n
}

// Clone returns a copy of the tree for a write to work on while readers keep
// using t. Every node of the tree proper is a new Node with its own Kids, so
// lists can be changed freely, but Fields maps are shared with t: call Own on a
// node before changing anything in its Fields. Envelopes are copied, including
// those of indexed embedded objects.
func (t *Tree) Clone() *Tree {
	c := &Tree{index: make(map[string]*Node, len(t.index)), deleted: t.deleted, owned: map[*Node]struct{}{}}
	copies := make(map[*Node]*Node, len(t.index))
	var copyNode func(n, parent *Node) *Node
	copyNode = func(n, parent *Node) *Node {
		out := &Node{ID: n.ID, Kind: n.Kind, Type: n.Type, Parent: parent, Env: n.Env,
			Fields: n.Fields, Kids: make(map[string][]*Node, len(n.Kids))}
		copies[n] = out
		if n.ID != "" && n.Kind.HasUUID() {
			c.index[n.ID] = out
		}
		for name, kids := range n.Kids {
			list := make([]*Node, len(kids))
			for i, k := range kids {
				list[i] = copyNode(k, out)
			}
			out.Kids[name] = list
		}
		return out
	}
	c.Root = copyNode(t.Root, nil)
	// The index is built during the copy rather than by a reindex walk: t's
	// IDs are already unique. Embedded objects keep their entries, re-pointed
	// at their owner's copy; they share its Fields until Own.
	for id, e := range t.index {
		if _, done := c.index[id]; done {
			continue
		}
		if owner := copies[e.Parent]; owner != nil {
			c.index[id] = &Node{ID: id, Kind: e.Kind, Parent: owner, Fields: e.Fields, Env: e.Env, Kids: map[string][]*Node{}}
		}
	}
	return c
}

// Own gives a node of a cloned tree its own deep copy of Fields, once, so it
// can be changed without touching the tree it was cloned from. Indexed
// embedded objects point into their owner's Fields, so Own is called on the
// owner, and Reindex afterwards points them at the copy.
func (t *Tree) Own(n *Node) {
	if t.owned == nil {
		t.owned = map[*Node]struct{}{}
	}
	if _, done := t.owned[n]; done {
		return
	}
	n.Fields = deepCopyMap(n.Fields)
	t.owned[n] = struct{}{}
}

// Owns marks a node as already owning its Fields, as a node built fresh from a
// request does.
func (t *Tree) Owns(n *Node) {
	if t.owned == nil {
		t.owned = map[*Node]struct{}{}
	}
	t.owned[n] = struct{}{}
}

// Reindex rebuilds the UUID index from the tree and reports duplicate IDs, which
// SPEC.md 7.2 forbids within a document.
func (t *Tree) Reindex() error {
	return t.reindexFrom(t.index)
}

// reindexFrom rebuilds the index, carrying envelopes of embedded objects over
// from old by ID, since those objects are rebuilt as new index entries.
func (t *Tree) reindexFrom(old map[string]*Node) error {
	t.index = map[string]*Node{}
	t.oldIndex = old
	defer func() { t.oldIndex = nil }()
	var dupes []string

	err := t.Root.Walk(func(n *Node) error {
		if n.ID == "" || !n.Kind.HasUUID() {
			return nil
		}
		if _, seen := t.index[n.ID]; seen {
			dupes = append(dupes, n.ID)
			return nil
		}
		t.index[n.ID] = n
		return nil
	})
	if err != nil {
		return err
	}

	// Nodes that are not split out still need indexing: an author, a note, a
	// bookmark and so on are addressable and must be unique too.
	if err := t.Root.Walk(func(n *Node) error {
		return t.indexEmbedded(n, &dupes)
	}); err != nil {
		return err
	}

	if len(dupes) > 0 {
		sort.Strings(dupes)
		return fmt.Errorf("model: duplicate UUIDs in document: %v", dedupe(dupes))
	}
	return nil
}

// embeddedLists names, per kind, the fields holding addressable children that
// are never split into their own records. They are indexed so that a UUID
// lookup finds them and duplicate detection covers them.
var embeddedLists = map[Kind][]struct {
	field string
	kind  Kind
}{
	KindDocument: {
		{"authors", KindAuthor},
		{"contributors", KindContributor},
		{"sources", KindSource},
		{"revisions", KindRevision},
	},
	KindScene:     {},
	KindElement:   {{"notes", KindNote}, {"revisions", KindRevision}},
	KindCharacter: {},
	KindAnalysis:  {{"passages", KindPassage}, {"summaries", KindSummary}},
}

// indexEmbedded indexes the addressable objects held inside a node's Fields.
func (t *Tree) indexEmbedded(n *Node, dupes *[]string) error {
	for _, list := range embeddedLists[n.Kind] {
		raw, present := n.Fields[list.field]
		if !present {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, ok := obj["id"].(string)
			if !ok || id == "" {
				continue
			}
			if _, seen := t.index[id]; seen {
				*dupes = append(*dupes, id)
				continue
			}
			// Embedded objects are indexed as lightweight nodes pointing at the
			// same map, so a write through the index changes the document.
			t.index[id] = t.embedded(id, list.kind, n, obj)
		}
	}

	// document.bookmarks sits under the "document" wrapper rather than at the
	// root, so it needs its own step.
	if n.Kind == KindDocument {
		if wrapper, ok := n.Fields["document"].(map[string]any); ok {
			if items, ok := wrapper["bookmarks"].([]any); ok {
				for _, item := range items {
					obj, ok := item.(map[string]any)
					if !ok {
						continue
					}
					id, ok := obj["id"].(string)
					if !ok || id == "" {
						continue
					}
					if _, seen := t.index[id]; seen {
						*dupes = append(*dupes, id)
						continue
					}
					t.index[id] = t.embedded(id, KindBookmark, n, obj)
				}
			}
		}
	}
	return nil
}

// embedded makes the index entry for an embedded object, keeping the envelope
// it had before the reindex.
func (t *Tree) embedded(id string, kind Kind, parent *Node, obj map[string]any) *Node {
	n := &Node{ID: id, Kind: kind, Parent: parent, Fields: obj, Kids: map[string][]*Node{}}
	if old := t.oldIndex[id]; old != nil && old.Kind == kind {
		n.Env = old.Env
	}
	return n
}

// Scenes returns the document's scenes in order.
func (t *Tree) Scenes() []*Node { return t.Root.Scenes() }

// SceneOf returns the scene a node belongs to, or nil.
func (t *Tree) SceneOf(n *Node) *Node {
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Kind == KindScene {
			return cur
		}
	}
	return nil
}

// Elements calls fn for every element in the document, in document order.
func (t *Tree) Elements(fn func(scene, el *Node) error) error {
	for _, scene := range t.Scenes() {
		for _, el := range scene.Body() {
			if err := fn(scene, el); err != nil {
				return err
			}
		}
	}
	return nil
}

// Counts returns the scene and element counts, for the document index (4.7).
func (t *Tree) Counts() (scenes, elements int) {
	for _, scene := range t.Scenes() {
		scenes++
		elements += len(scene.Body())
	}
	return scenes, elements
}

// sortedKeys returns a map's keys in order, for deterministic output over maps
// the schema gives no order to.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dedupe removes repeats from a sorted slice.
func dedupe(in []string) []string {
	out := in[:0:0]
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// Each calls fn for every indexed node — tree nodes and embedded objects with
// a UUID — in no particular order.
func (t *Tree) Each(fn func(*Node)) {
	for _, n := range t.index {
		fn(n)
	}
}
