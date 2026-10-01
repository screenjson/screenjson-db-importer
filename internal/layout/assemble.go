package layout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/order"
	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Repair is something loading found wrong and fixed in the assembled tree
// (R-ARCH-10). The caller logs it at warn level, counts it in
// screenjson_repairs_total, and writes the fix back: Delete removes the record,
// otherwise the record of the node ID names is rewritten from the tree.
type Repair struct {
	// Code is the kind of repair: orphan, duplicate_order or scene_field.
	Code string
	// Collection is where the record lives.
	Collection string
	// ID is the record.
	ID string
	// Delete says the record should be removed rather than rewritten.
	Delete bool
	// Message says what was found.
	Message string
}

// The repair codes.
const (
	RepairOrphan         = "orphan"
	RepairDuplicateOrder = "duplicate_order"
	RepairSceneField     = "scene_field"
)

// Assemble puts a document back together from its records, keyed by level.
//
// Siblings are sorted here by order key and then ID, never by the driver
// (R-STORE-10). Natively stored embeddings go back into analysis.embeddings at
// the place they came from, so reads always return standard ScreenJSON
// (R-STORE-13). Records whose parent is missing are left out, an element whose
// "scene" field names the wrong scene is corrected, and a sibling list with
// missing or duplicate order keys is re-keyed; each is reported as a Repair.
//
// Nodes the layout keeps embedded have no envelope of their own. They get
// evenly spread order keys, and revision numbers equal to the document's seq:
// every change to a node is a write to the document, so no node's rev can
// exceed seq, and starting from seq means a rev is never handed out twice for
// different content.
func (l Layout) Assemble(doc string, recs map[Level][]store.Record) (*model.Tree, []Repair, error) {
	var repairs []Repair
	var rootRec *store.Record
	for i := range recs[Root] {
		if recs[Root][i].ID == doc {
			rootRec = &recs[Root][i]
		}
	}
	if rootRec == nil {
		return nil, nil, ErrNotFound
	}
	root, err := decode(rootRec.Node)
	if err != nil {
		return nil, nil, fmt.Errorf("layout: root of %s: %w", doc, err)
	}

	// byID collects every split record for setting envelopes afterwards.
	byID := map[string]*store.Record{}
	rekey := map[string]bool{}

	if l.Splits(model.KindScene) {
		sceneRecs := l.children(recs[Scenes], doc, string(model.KindScene), &repairs)
		sceneIDs := map[string]bool{}
		for _, r := range sceneRecs {
			sceneIDs[r.ID] = true
		}

		elemsOf := map[string][]*store.Record{}
		if l.Splits(model.KindElement) {
			for i := range recs[Elements] {
				r := &recs[Elements][i]
				if r.Doc != doc || r.Kind != string(model.KindElement) {
					continue
				}
				if !sceneIDs[r.Parent] {
					repairs = append(repairs, Repair{Code: RepairOrphan, Collection: l[Elements].Collection,
						ID: r.ID, Delete: true, Message: fmt.Sprintf("element's scene %s does not exist", r.Parent)})
					continue
				}
				elemsOf[r.Parent] = append(elemsOf[r.Parent], r)
			}
		}

		scenes := make([]any, 0, len(sceneRecs))
		if checkOrder(sceneRecs) {
			rekey[doc+"/scenes"] = true
		}
		for _, sr := range sceneRecs {
			byID[sr.ID] = sr
			scene, err := decode(sr.Node)
			if err != nil {
				return nil, nil, fmt.Errorf("layout: scene %s: %w", sr.ID, err)
			}
			if l.Splits(model.KindElement) {
				elems := elemsOf[sr.ID]
				sortRecords(elems)
				if checkOrder(elems) {
					rekey[sr.ID] = true
				}
				body := make([]any, 0, len(elems))
				for _, er := range elems {
					byID[er.ID] = er
					el, err := decode(er.Node)
					if err != nil {
						return nil, nil, fmt.Errorf("layout: element %s: %w", er.ID, err)
					}
					if el["scene"] != sr.ID {
						el["scene"] = sr.ID
						repairs = append(repairs, Repair{Code: RepairSceneField, Collection: l[Elements].Collection,
							ID: er.ID, Message: fmt.Sprintf("element's scene field did not name its parent %s", sr.ID)})
					}
					body = append(body, el)
				}
				scene["body"] = body
			}
			scenes = append(scenes, scene)
		}
		wrapper, _ := root["document"].(map[string]any)
		if wrapper == nil {
			wrapper = map[string]any{}
			root["document"] = wrapper
		}
		wrapper["scenes"] = scenes
	}

	if l.Splits(model.KindCharacter) {
		charRecs := l.children(recs[Characters], doc, string(model.KindCharacter), &repairs)
		if checkOrder(charRecs) {
			rekey[doc+"/characters"] = true
		}
		chars := make([]any, 0, len(charRecs))
		for _, cr := range charRecs {
			byID[cr.ID] = cr
			c, err := decode(cr.Node)
			if err != nil {
				return nil, nil, fmt.Errorf("layout: character %s: %w", cr.ID, err)
			}
			chars = append(chars, c)
		}
		// The root record keeps an empty "characters" when the list was
		// present, so an empty list is told apart from an absent one.
		if _, present := root["characters"]; present || len(chars) > 0 {
			root["characters"] = chars
		}
	}

	var analysisRec *store.Record
	if l.Splits(model.KindAnalysis) {
		for i := range recs[Analysis] {
			if r := &recs[Analysis][i]; r.ID == doc && r.Doc == doc {
				analysisRec = r
			}
		}
		if analysisRec != nil {
			a, err := decode(analysisRec.Node)
			if err != nil {
				return nil, nil, fmt.Errorf("layout: analysis of %s: %w", doc, err)
			}
			root["analysis"] = a
		}
	}

	if err := restoreNatives(root, byID); err != nil {
		return nil, nil, err
	}

	tree, err := model.FromMap(root)
	if err != nil {
		return nil, nil, fmt.Errorf("layout: assemble %s: %w", doc, err)
	}

	seq := rootRec.Seq
	if seq == 0 {
		seq = 1
	}
	tree.Root.Env = envelope(rootRec, seq)
	if a := tree.Root.Analysis(); a != nil && analysisRec != nil {
		a.Env = envelope(analysisRec, seq)
	}

	// Split nodes take their envelope from their record; embedded ones get
	// the defaults described above.
	var keyErr error
	fill := func(parent *model.Node, kids []*model.Node, listKey string) {
		for _, kid := range kids {
			if r, ok := byID[kid.ID]; ok {
				kid.Env = envelope(r, seq)
			} else {
				kid.Env = model.Envelope{Rev: seq, TextRev: seq, CRev: seq, Updated: rootRec.Updated}
			}
		}
		split := len(kids) > 0 && byID[kids[0].ID] != nil
		if split && !rekey[listKey] {
			return
		}
		keys, err := order.Spread(len(kids))
		if err != nil {
			keyErr = err
			return
		}
		for i, kid := range kids {
			kid.Env.Order = keys[i]
			if split {
				repairs = append(repairs, Repair{Code: RepairDuplicateOrder, Collection: l.Collection(kid.Kind), ID: kid.ID,
					Message: fmt.Sprintf("siblings under %s had missing or duplicate order keys and were re-keyed", parent.ID)})
			}
		}
	}
	fill(tree.Root, tree.Root.Characters(), doc+"/characters")
	fill(tree.Root, tree.Scenes(), doc+"/scenes")
	for _, s := range tree.Scenes() {
		fill(s, s.Body(), s.ID)
	}
	if keyErr != nil {
		return nil, nil, fmt.Errorf("layout: spread order keys: %w", keyErr)
	}
	return tree, repairs, nil
}

// children picks a level's records that belong directly under the root,
// sorted, reporting any whose parent isn't the document.
func (l Layout) children(recs []store.Record, doc, kind string, repairs *[]Repair) []*store.Record {
	var out []*store.Record
	for i := range recs {
		r := &recs[i]
		if r.Doc != doc || r.Kind != kind {
			continue
		}
		if r.Parent != doc {
			lv, _ := LevelOf(model.Kind(kind))
			*repairs = append(*repairs, Repair{Code: RepairOrphan, Collection: l[lv].Collection, ID: r.ID,
				Delete: true, Message: fmt.Sprintf("%s's parent %s is not its document", kind, r.Parent)})
			continue
		}
		out = append(out, r)
	}
	sortRecords(out)
	return out
}

// sortRecords sorts siblings by order key, byte by byte, then by ID so that
// duplicates still come out the same way every time.
func sortRecords(recs []*store.Record) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Order != recs[j].Order {
			return recs[i].Order < recs[j].Order
		}
		return recs[i].ID < recs[j].ID
	})
}

// checkOrder reports whether sorted siblings need re-keying: a missing, invalid
// or duplicate key.
func checkOrder(recs []*store.Record) bool {
	for i, r := range recs {
		if !order.Valid(r.Order) {
			return true
		}
		if i > 0 && recs[i-1].Order == r.Order {
			return true
		}
	}
	return false
}

func envelope(r *store.Record, seq uint64) model.Envelope {
	env := model.Envelope{
		Order: r.Order, Rev: r.Rev, TextRev: r.TextRev, CRev: r.CRev,
		Seq: r.Seq, Updated: r.Updated, Layout: r.Layout,
	}
	// A record written without an envelope (one being adopted, R-STORE-05)
	// starts at the same point embedded nodes do.
	if env.Rev == 0 {
		env.Rev = seq
	}
	if env.TextRev == 0 {
		env.TextRev = seq
	}
	if env.CRev == 0 {
		env.CRev = seq
	}
	return env
}

// restoreNatives puts natively stored embeddings back into
// analysis.embeddings, each at the index it was taken from.
func restoreNatives(root map[string]any, byID map[string]*store.Record) error {
	type entry struct {
		id    string
		index int
		value map[string]any
	}
	var entries []entry
	for id, r := range byID {
		if r.Vector == nil || len(r.VectorMeta) == 0 {
			continue
		}
		var vm vectorMeta
		dec := json.NewDecoder(bytes.NewReader(r.VectorMeta))
		dec.UseNumber()
		if err := dec.Decode(&vm); err != nil {
			return fmt.Errorf("layout: vector meta of %s: %w", id, err)
		}
		e := vm.Embedding
		if e == nil {
			e = map[string]any{}
		}
		values := make([]any, len(r.Vector))
		for i, f := range r.Vector {
			values[i] = json.Number(FormatFloat32(f))
		}
		e["values"] = values
		if _, ok := e["model"]; !ok {
			e["model"] = r.VectorModel
		}
		entries = append(entries, entry{id: id, index: vm.Index, value: e})
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	a, _ := root["analysis"].(map[string]any)
	if a == nil {
		a = map[string]any{}
		root["analysis"] = a
	}
	emb, _ := a["embeddings"].(map[string]any)
	if emb == nil {
		emb = map[string]any{}
		a["embeddings"] = emb
	}
	for _, e := range entries {
		list, _ := emb[e.id].([]any)
		i := e.index
		if i < 0 || i > len(list) {
			i = len(list)
		}
		list = append(list, nil)
		copy(list[i+1:], list[i:])
		list[i] = e.value
		emb[e.id] = list
	}
	return nil
}

// decode reads a record's ScreenJSON, keeping numbers as written.
func decode(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("record holds no object")
	}
	return m, nil
}
