package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// Envelope is the server's bookkeeping for one node. It is stored next to the
// node in its record and never inside the ScreenJSON (SPEC.md R-SJ-01).
type Envelope struct {
	// Order is the key that sorts this node among its siblings. Empty on the
	// root and on nodes whose parent holds no ordered list.
	Order string
	// Rev goes up by one on every change to this node.
	Rev uint64
	// TextRev goes up only when the node's text map changes.
	TextRev uint64
	// CRev goes up when this node's children are inserted, removed or reordered.
	CRev uint64
	// Seq is the document's sequence number. Root only.
	Seq uint64
	// Updated is when this node was last written.
	Updated time.Time
	// Layout is the layout fingerprint the document was written with. Root only.
	Layout string
}

// childField names a list of children that the layout may store as separate
// records, and says where that list sits in the node's JSON.
type childField struct {
	// name is how the list is addressed inside the server.
	name string
	// path is where the list lives in the node's ScreenJSON, as field names from
	// the node's root. Scenes are nested under "document", the rest are direct.
	path []string
	// kind is what the children are.
	kind Kind
	// single marks a lone object rather than an array.
	single bool
}

// childFields lists, per kind, the children that can become their own records
// (SPEC.md 5.2). Every other child stays inside its parent's Fields map.
var childFields = map[Kind][]childField{
	KindDocument: {
		{name: "scenes", path: []string{"document", "scenes"}, kind: KindScene},
		{name: "characters", path: []string{"characters"}, kind: KindCharacter},
		{name: "analysis", path: []string{"analysis"}, kind: KindAnalysis, single: true},
	},
	KindScene: {
		{name: "body", path: []string{"body"}, kind: KindElement},
	},
}

// ChildFieldNames returns the names of a kind's splittable child lists.
func ChildFieldNames(k Kind) []string {
	fields := childFields[k]
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.name)
	}
	return out
}

// Node is one addressable part of a document.
//
// Fields holds the node's own ScreenJSON with its splittable children removed;
// those live in Kids. Assembling the two gives back the complete node
// (see JSON). Nodes are generic JSON rather than typed structs, for the reasons
// in docs/DECISIONS.md D-006.
type Node struct {
	// ID is the node's UUID, or its slug for a color, or "{authority}~{id}" for
	// a registration. Empty for analysis, which is a singleton.
	ID string
	// Kind is what this node is.
	Kind Kind
	// Type is the element type, for elements only.
	Type ElementType
	// Parent is the node this one hangs from. Nil on the root.
	Parent *Node
	// Env is the server's bookkeeping.
	Env Envelope
	// Fields is the node's own ScreenJSON, without the children in Kids.
	Fields map[string]any
	// Kids holds the splittable children, by child-field name, in document order.
	Kids map[string][]*Node
}

// NewNode makes an empty node of a kind.
func NewNode(kind Kind) *Node {
	return &Node{
		Kind:   kind,
		Fields: map[string]any{},
		Kids:   map[string][]*Node{},
	}
}

// Child returns the child list by field name.
func (n *Node) Child(name string) []*Node {
	if n == nil {
		return nil
	}
	return n.Kids[name]
}

// SetChildren replaces a child list and points each child's Parent at n.
func (n *Node) SetChildren(name string, kids []*Node) {
	if n.Kids == nil {
		n.Kids = map[string][]*Node{}
	}
	for _, k := range kids {
		k.Parent = n
	}
	n.Kids[name] = kids
}

// Scenes returns the document's scenes, in order.
func (n *Node) Scenes() []*Node { return n.Child("scenes") }

// Body returns a scene's elements, in order.
func (n *Node) Body() []*Node { return n.Child("body") }

// Characters returns the document's characters, in order.
func (n *Node) Characters() []*Node { return n.Child("characters") }

// Analysis returns the document's analysis node, or nil.
func (n *Node) Analysis() *Node {
	if kids := n.Child("analysis"); len(kids) > 0 {
		return kids[0]
	}
	return nil
}

// Doc walks up to the root and returns its ID, which is the document's UUID.
func (n *Node) Doc() string {
	cur := n
	for cur.Parent != nil {
		cur = cur.Parent
	}
	return cur.ID
}

// Text returns the node's text map, and whether it has one.
//
// A character cue has none: its only wording is "display", a plain string of at
// most 120 characters rather than a language map (SPEC.md 3.3). Text therefore
// reports false for every cue, which is what features reading "the element's
// text" need.
func (n *Node) Text() (map[string]any, bool) {
	return n.langMap("text")
}

// langMap returns a language-keyed map field.
func (n *Node) langMap(field string) (map[string]any, bool) {
	raw, present := n.Fields[field]
	if !present {
		return nil, false
	}
	m, isMap := raw.(map[string]any)
	if !isMap {
		return nil, false
	}
	return m, true
}

// Langs returns the languages a node's text map holds, sorted.
func (n *Node) Langs() []string {
	m, ok := n.Text()
	if !ok {
		return nil
	}
	return sortedKeys(m)
}

// String gives a short description for logs and errors.
func (n *Node) String() string {
	if n == nil {
		return "<nil node>"
	}
	if n.Type != "" {
		return fmt.Sprintf("%s/%s %s", n.Kind, n.Type, n.ID)
	}
	return fmt.Sprintf("%s %s", n.Kind, n.ID)
}

// JSON assembles the node and everything under it back into ScreenJSON.
//
// It is the inverse of the split that Fields and Kids represent: each child list
// is written back at the path childFields gives it, so a document that was taken
// apart across five collections comes back as one file (R-STORE-13, R-VER-02).
func (n *Node) JSON() (map[string]any, error) {
	return n.JSONExcept(nil)
}

// JSONExcept is JSON with some child kinds left out, which is how a layout
// builds a record: children stored as records of their own are not repeated
// in their parent's (SPEC.md 5.1). A left-out list is written as an empty
// array when it was present, so its presence survives the split; a left-out
// single child (analysis) is dropped, since its own record marks it present.
// A nil split leaves nothing out.
func (n *Node) JSONExcept(split func(Kind) bool) (map[string]any, error) {
	out := make(map[string]any, len(n.Fields)+len(n.Kids))
	for k, v := range n.Fields {
		out[k] = v
	}

	for _, field := range childFields[n.Kind] {
		// A list is written back whenever it was present, even empty: an
		// explicit "characters": [] or a scene's required "body": [] must
		// survive a round trip. An absent list stays absent.
		kids, present := n.Kids[field.name]
		if !present {
			continue
		}
		leftOut := split != nil && split(field.kind)
		if field.single {
			if len(kids) == 0 || leftOut {
				continue
			}
			sub, err := kids[0].JSONExcept(split)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", field.name, err)
			}
			if err := setPathCopy(out, field.path, sub); err != nil {
				return nil, err
			}
			continue
		}
		items := make([]any, 0, len(kids))
		if !leftOut {
			for i, kid := range kids {
				sub, err := kid.JSONExcept(split)
				if err != nil {
					return nil, fmt.Errorf("%s[%d]: %w", field.name, i, err)
				}
				items = append(items, sub)
			}
		}
		if err := setPathCopy(out, field.path, items); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ChildPath returns where a splittable child list of a kind sits in the
// node's ScreenJSON, as field names, and whether the kind has such a list of
// children of that kind.
func ChildPath(parent, child Kind) ([]string, bool) {
	for _, f := range childFields[parent] {
		if f.kind == child {
			out := make([]string, len(f.path))
			copy(out, f.path)
			return out, true
		}
	}
	return nil, false
}

// MarshalJSON lets a node be encoded directly.
func (n *Node) MarshalJSON() ([]byte, error) {
	v, err := n.JSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// setPathCopy writes a value at a nested field path of a freshly built map,
// copying every intermediate object on the way. The intermediate objects may
// belong to a node's Fields — "document" on the root — and writing into them
// would change the tree itself, and race with other readers.
func setPathCopy(root map[string]any, path []string, val any) error {
	if len(path) == 0 {
		return fmt.Errorf("model: empty child path")
	}
	cur := root
	for _, step := range path[:len(path)-1] {
		copied := map[string]any{}
		if next, present := cur[step]; present {
			asMap, ok := next.(map[string]any)
			if !ok {
				return fmt.Errorf("model: %q is not an object, so children cannot be written under it", step)
			}
			for k, v := range asMap {
				copied[k] = v
			}
		}
		cur[step] = copied
		cur = copied
	}
	cur[path[len(path)-1]] = val
	return nil
}

// takePath removes and returns a value at a nested field path.
func takePath(root map[string]any, path []string) (any, bool) {
	cur := root
	for _, step := range path[:len(path)-1] {
		next, present := cur[step]
		if !present {
			return nil, false
		}
		asMap, ok := next.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = asMap
	}
	last := path[len(path)-1]
	val, present := cur[last]
	if !present {
		return nil, false
	}
	delete(cur, last)
	return val, true
}

// Walk calls fn for n and every node beneath it, parents before children and
// siblings in document order. A non-nil error from fn stops the walk.
func (n *Node) Walk(fn func(*Node) error) error {
	if n == nil {
		return nil
	}
	if err := fn(n); err != nil {
		return err
	}
	for _, field := range childFields[n.Kind] {
		for _, kid := range n.Kids[field.name] {
			if err := kid.Walk(fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Clone makes a deep copy, so a cached tree can be handed out without a caller
// being able to change it.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	out := &Node{
		ID:     n.ID,
		Kind:   n.Kind,
		Type:   n.Type,
		Env:    n.Env,
		Fields: deepCopyMap(n.Fields),
		Kids:   make(map[string][]*Node, len(n.Kids)),
	}
	for name, kids := range n.Kids {
		copied := make([]*Node, len(kids))
		for i, kid := range kids {
			copied[i] = kid.Clone()
			copied[i].Parent = out
		}
		out.Kids[name] = copied
	}
	return out
}

// deepCopyMap copies a decoded JSON object.
func deepCopyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue copies a decoded JSON value.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		// Scalars decoded from JSON are immutable values.
		return v
	}
}
