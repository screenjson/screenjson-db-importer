package layout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/order"
	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Put is one record to write, and where.
type Put struct {
	Collection string
	Record     store.Record
}

// Owner returns the node whose record holds n: n itself when its kind is split
// out, otherwise its nearest ancestor that is. Embedded objects indexed by the
// tree (authors, notes and so on) are owned the same way, through Parent.
func (l Layout) Owner(n *model.Node) *model.Node {
	cur := n
	if cur != nil && !isTreeNode(cur) {
		cur = cur.Parent
	}
	for ; cur != nil; cur = cur.Parent {
		if cur.Kind == model.KindDocument || l.Splits(cur.Kind) {
			return cur
		}
	}
	return nil
}

// isTreeNode tells a node of the tree proper from a lightweight index entry
// for an embedded object: only the splittable kinds form the tree.
func isTreeNode(n *model.Node) bool {
	_, ok := LevelOf(n.Kind)
	return ok
}

// RecordID is the ID of the record a split node is stored as. The analysis
// node has no UUID, so its record takes the document's; it can't collide,
// because every level has its own collection.
func RecordID(n *model.Node) string {
	if n.Kind == model.KindAnalysis {
		return n.Doc()
	}
	return n.ID
}

// Records takes a whole document apart into its records.
//
// Commit write rule (R-ARCH-05): the root record comes last. Loading starts
// from the root, so until it is written the document doesn't exist, and a
// crash part-way through an import leaves only orphans, which loading ignores
// and fsck removes.
func (l Layout) Records(t *model.Tree) ([]Put, error) {
	nat := l.natives(t)
	var out []Put
	err := t.Root.Walk(func(n *model.Node) error {
		if n.Kind == model.KindDocument || !l.Splits(n.Kind) {
			return nil
		}
		p, err := l.record(t, n, nat)
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Group by level, deepest first — elements, then scenes, characters and
	// analysis — so each collection is written in one batch, and a scene is
	// written only after its elements.
	rank := map[model.Kind]int{model.KindElement: 0, model.KindScene: 1, model.KindCharacter: 2, model.KindAnalysis: 3}
	sort.SliceStable(out, func(i, j int) bool {
		return rank[model.Kind(out[i].Record.Kind)] < rank[model.Kind(out[j].Record.Kind)]
	})
	root, err := l.record(t, t.Root, nat)
	if err != nil {
		return nil, err
	}
	return append(out, root), nil
}

// Record builds the record for one node, which must be the root or of a split
// kind (use Owner to find it).
func (l Layout) Record(t *model.Tree, n *model.Node) (Put, error) {
	if n.Kind != model.KindDocument && !l.Splits(n.Kind) {
		return Put{}, fmt.Errorf("layout: %s is not stored as a record of its own", n)
	}
	return l.record(t, n, l.natives(t))
}

// RecordsOf builds the records for several nodes, each of which must be the
// root or of a split kind, in the order given.
func (l Layout) RecordsOf(t *model.Tree, nodes []*model.Node) ([]Put, error) {
	nat := l.natives(t)
	out := make([]Put, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind != model.KindDocument && !l.Splits(n.Kind) {
			return nil, fmt.Errorf("layout: %s is not stored as a record of its own", n)
		}
		p, err := l.record(t, n, nat)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (l Layout) record(t *model.Tree, n *model.Node, nat map[string]native) (Put, error) {
	js, err := n.JSONExcept(l.Splits)
	if err != nil {
		return Put{}, fmt.Errorf("layout: %s: %w", n, err)
	}
	if l.holdsAnalysis(n) {
		stripNatives(n, js, nat)
	}
	body, err := json.Marshal(js)
	if err != nil {
		return Put{}, fmt.Errorf("layout: encode %s: %w", n, err)
	}

	lang, _ := t.Root.Fields["lang"].(string)
	r := store.Record{
		ID:      RecordID(n),
		Doc:     t.Doc(),
		Kind:    string(n.Kind),
		Type:    string(n.Type),
		Order:   n.Env.Order,
		Rev:     n.Env.Rev,
		TextRev: n.Env.TextRev,
		CRev:    n.Env.CRev,
		Updated: n.Env.Updated,
		Node:    body,
		Text:    SearchText(n, lang),
	}
	if n.Parent != nil {
		r.Parent = RecordID(n.Parent)
	}
	if n.Kind == model.KindDocument {
		r.Seq, r.Layout, r.Order = n.Env.Seq, n.Env.Layout, ""
	}
	if nv, ok := nat[n.ID]; ok && n.ID != "" {
		meta, err := json.Marshal(vectorMeta{Index: nv.index, Embedding: nv.meta})
		if err != nil {
			return Put{}, fmt.Errorf("layout: encode vector meta of %s: %w", n, err)
		}
		r.Vector, r.VectorModel, r.VectorMeta = nv.values, nv.model, meta
	}
	col := l.Collection(n.Kind)
	return Put{Collection: col, Record: r}, nil
}

// holdsAnalysis reports whether n's record is where analysis is stored.
func (l Layout) holdsAnalysis(n *model.Node) bool {
	if l.Splits(model.KindAnalysis) {
		return n.Kind == model.KindAnalysis
	}
	return n.Kind == model.KindDocument
}

// native is one embedding stored in its node's native vector slot rather than
// in analysis (SPEC.md 5.5).
type native struct {
	model  string
	index  int
	values []float32
	meta   map[string]any
}

// vectorMeta is what Record.VectorMeta holds: the embedding without its values,
// and its place in the node's list (docs/DECISIONS.md D-008).
type vectorMeta struct {
	Index     int            `json:"index"`
	Embedding map[string]any `json:"embedding"`
}

// natives finds the embeddings stored natively: for a node whose level is
// split and declares a vector, the first embedding for that model whose values
// are numbers of the declared length. Anything else stays in analysis, so an
// embedding is never lost for not fitting the native slot.
func (l Layout) natives(t *model.Tree) map[string]native {
	out := map[string]native{}
	a := t.Root.Analysis()
	if a == nil {
		return out
	}
	emb, _ := a.Fields["embeddings"].(map[string]any)
	for id, raw := range emb {
		n := t.Node(id)
		if n == nil || !isTreeNode(n) {
			continue
		}
		vec := l.VectorFor(n.Kind)
		if vec == nil {
			continue
		}
		list, _ := raw.([]any)
		for i, item := range list {
			e, ok := item.(map[string]any)
			if !ok || e["model"] != vec.Model {
				continue
			}
			values, ok := floats(e["values"])
			if !ok || len(values) != vec.Dimensions {
				break
			}
			meta := make(map[string]any, len(e))
			for k, v := range e {
				if k != "values" {
					meta[k] = v
				}
			}
			out[id] = native{model: vec.Model, index: i, values: values, meta: meta}
			break
		}
	}
	return out
}

// stripNatives removes natively stored embeddings from the JSON of the record
// holding analysis. js is fresh from JSONExcept, but the embeddings map inside
// it is still the tree's, so it is copied before changing.
func stripNatives(n *model.Node, js map[string]any, nat map[string]native) {
	a := js
	if n.Kind == model.KindDocument {
		sub, ok := js["analysis"].(map[string]any)
		if !ok {
			return
		}
		a = make(map[string]any, len(sub))
		for k, v := range sub {
			a[k] = v
		}
		js["analysis"] = a
	}
	emb, ok := a["embeddings"].(map[string]any)
	if !ok || len(nat) == 0 {
		return
	}
	out := make(map[string]any, len(emb))
	for id, raw := range emb {
		nv, isNative := nat[id]
		list, _ := raw.([]any)
		if !isNative {
			out[id] = raw
			continue
		}
		kept := make([]any, 0, len(list))
		for i, item := range list {
			if i != nv.index {
				kept = append(kept, item)
			}
		}
		// An empty list would break the schema's minItems 1; the key comes
		// back when the native embedding is put back on load.
		if len(kept) > 0 {
			out[id] = kept
		}
	}
	a["embeddings"] = out
}

// floats converts a JSON array of numbers to float32s.
func floats(v any) ([]float32, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]float32, len(list))
	for i, item := range list {
		var f float64
		switch x := item.(type) {
		case json.Number:
			parsed, err := x.Float64()
			if err != nil {
				return nil, false
			}
			f = parsed
		case float64:
			f = x
		default:
			return nil, false
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		out[i] = float32(f)
	}
	return out, true
}

// FormatFloat32 writes a vector component the way native vectors come back:
// the shortest decimal that reads back as the same float32. A fixture whose
// values are written this way round-trips byte for byte (D-008).
func FormatFloat32(f float32) string {
	return strconv.FormatFloat(float64(f), 'g', -1, 32)
}

// SearchText is the plain text a record is searched by (SPEC.md 5.1): the
// node's text in the document's primary language. A cue has no text map, so
// its display string is used; a scene is found by its heading, a character by
// its name and the document by its title.
func SearchText(n *model.Node, lang string) string {
	// Text search skips encrypted text (R-ENC-02): the node's own encrypt,
	// or the document's.
	for cur := n; cur != nil; cur = cur.Parent {
		if v, ok := cur.Fields["encrypt"]; ok && v != nil {
			return ""
		}
	}
	switch n.Kind {
	case model.KindElement:
		if n.Type == model.TypeCharacter {
			s, _ := n.Fields["display"].(string)
			return s
		}
		return pick(n.Fields["text"], lang)
	case model.KindScene:
		return Slugline(n.Fields["heading"])
	case model.KindCharacter:
		s, _ := n.Fields["name"].(string)
		return s
	case model.KindDocument:
		return pick(n.Fields["title"], lang)
	}
	return ""
}

// Slugline writes a scene heading as a script shows it: "INT. KITCHEN - DAY".
func Slugline(v any) string {
	h, _ := v.(map[string]any)
	context, _ := h["context"].(string)
	setting, _ := h["setting"].(string)
	tod, _ := h["time"].(string)
	var b strings.Builder
	if context != "" {
		b.WriteString(context)
		b.WriteString(". ")
	}
	b.WriteString(setting)
	if tod != "" {
		b.WriteString(" - ")
		b.WriteString(tod)
	}
	return strings.TrimSpace(b.String())
}

// pick chooses one language from a text map: the exact tag, then the same base
// language ("en" for "en-GB" or the reverse), then the first tag in sorted
// order so the choice never depends on map order.
func pick(v any, lang string) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	if s, ok := m[lang].(string); ok {
		return s
	}
	base, _, _ := strings.Cut(strings.ToLower(lang), "-")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kb, _, _ := strings.Cut(strings.ToLower(k), "-")
		if kb == base {
			s, _ := m[k].(string)
			return s
		}
	}
	s, _ := m[keys[0]].(string)
	return s
}

// ErrNotFound means the document has no root record.
var ErrNotFound = errors.New("layout: document not found")

// Save writes a whole document: every record, children before the root (see
// Records for why). On drivers with transactions it is one transaction.
func (l Layout) Save(ctx context.Context, d store.Driver, t *model.Tree) error {
	puts, err := l.Records(t)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(ctx context.Context) error {
		return WritePuts(ctx, d, puts)
	})
}

// WritePuts writes records in the order given, batching consecutive records
// for the same collection, so the caller's commit order is kept.
func WritePuts(ctx context.Context, d store.Driver, puts []Put) error {
	for i := 0; i < len(puts); {
		j := i
		batch := []store.Record{}
		for j < len(puts) && puts[j].Collection == puts[i].Collection {
			batch = append(batch, puts[j].Record)
			j++
		}
		if err := d.Put(ctx, puts[i].Collection, batch); err != nil {
			return fmt.Errorf("layout: write %d records to %s: %w", len(batch), puts[i].Collection, err)
		}
		i = j
	}
	return nil
}

// Delete removes a document's records.
//
// Commit write rule (R-ARCH-05): the root record goes first, which is what
// makes the document gone. The other records are cleanup; any a crash leaves
// behind are orphans, ignored on load and removed by fsck.
func (l Layout) Delete(ctx context.Context, d store.Driver, doc string) error {
	return d.Tx(ctx, func(ctx context.Context) error {
		if err := d.Delete(ctx, l[Root].Collection, []string{doc}); err != nil {
			return fmt.Errorf("layout: delete root of %s: %w", doc, err)
		}
		for _, lv := range levels[1:] {
			spec, ok := l[lv]
			if !ok {
				continue
			}
			if err := d.DeleteWhere(ctx, spec.Collection, store.Filter{Doc: doc}); err != nil {
				return fmt.Errorf("layout: delete %s of %s: %w", lv, doc, err)
			}
		}
		return nil
	})
}

// Load reads a document's records and assembles it.
func (l Layout) Load(ctx context.Context, d store.Driver, doc string) (*model.Tree, []Repair, error) {
	roots, err := d.Get(ctx, l[Root].Collection, []string{doc})
	if err != nil {
		return nil, nil, fmt.Errorf("layout: load root of %s: %w", doc, err)
	}
	if len(roots) == 0 {
		return nil, nil, ErrNotFound
	}
	recs := map[Level][]store.Record{Root: roots}
	for _, lv := range levels[1:] {
		spec, ok := l[lv]
		if !ok {
			continue
		}
		found, err := store.FindAll(ctx, d, spec.Collection, store.Filter{Doc: doc})
		if err != nil {
			return nil, nil, fmt.Errorf("layout: load %s of %s: %w", lv, doc, err)
		}
		recs[lv] = found
	}
	return l.Assemble(doc, recs)
}

// AssignOrderKeys gives evenly spread keys (SPEC.md 5.4) to every sibling list
// in which any node lacks a key: scenes, each scene's elements, and characters.
// Import calls it before the first save; lists already keyed are left alone.
func AssignOrderKeys(t *model.Tree) error {
	lists := [][]*model.Node{t.Root.Characters(), t.Scenes()}
	for _, s := range t.Scenes() {
		lists = append(lists, s.Body())
	}
	for _, kids := range lists {
		missing := false
		for _, k := range kids {
			if k.Env.Order == "" {
				missing = true
			}
		}
		if !missing {
			continue
		}
		keys, err := order.Spread(len(kids))
		if err != nil {
			return fmt.Errorf("layout: spread order keys: %w", err)
		}
		for i, k := range kids {
			k.Env.Order = keys[i]
		}
	}
	return nil
}
