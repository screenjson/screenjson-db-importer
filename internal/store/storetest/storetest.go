// Package storetest is the conformance suite every driver passes (SPEC.md
// 17.3, R-STORE-14). A driver's own test calls Run with a factory.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Dimensions is the size of the vectors the suite stores.
const Dimensions = 4

// Model is the native vector model the suite declares.
const Model = "storetest-model"

// Factory returns a connected driver. It is called once per subtest; the suite
// closes the driver when the subtest ends.
type Factory func(t *testing.T) store.Driver

// Run runs the whole suite. Every subtest works in collections with a random
// prefix, so the suite can run repeatedly against a live database; it deletes
// its records when it's done.
func Run(t *testing.T, newDriver Factory) {
	t.Run("EnsureTwice", func(t *testing.T) { testEnsureTwice(t, newDriver) })
	t.Run("PutGet", func(t *testing.T) { testPutGet(t, newDriver) })
	t.Run("Upsert", func(t *testing.T) { testUpsert(t, newDriver) })
	t.Run("FindFilters", func(t *testing.T) { testFindFilters(t, newDriver) })
	t.Run("FindPaginates", func(t *testing.T) { testFindPaginates(t, newDriver) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, newDriver) })
	t.Run("Vectors", func(t *testing.T) { testVectors(t, newDriver) })
	t.Run("SearchText", func(t *testing.T) { testSearchText(t, newDriver) })
	t.Run("SearchVector", func(t *testing.T) { testSearchVector(t, newDriver) })
	t.Run("Unicode", func(t *testing.T) { testUnicode(t, newDriver) })
	t.Run("Tx", func(t *testing.T) { testTx(t, newDriver) })
}

// env is one subtest's driver and collection.
type env struct {
	t   *testing.T
	ctx context.Context
	d   store.Driver
	col string
}

func setup(t *testing.T, newDriver Factory) *env {
	t.Helper()
	d := newDriver(t)
	e := &env{t: t, ctx: context.Background(), d: d, col: "sjtest_" + randHex(4) + "_elements"}
	if err := d.Ensure(e.ctx, e.specs()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	t.Cleanup(func() {
		_ = d.DeleteWhere(context.Background(), e.col, store.Filter{})
		_ = d.Close()
	})
	return e
}

func (e *env) specs() []store.CollectionSpec {
	return []store.CollectionSpec{{
		Name: e.col, Level: "elements", FullText: true,
		Vector: &store.VectorSpec{Model: Model, Dimensions: Dimensions},
	}}
}

func (e *env) put(recs ...store.Record) {
	e.t.Helper()
	if err := e.d.Put(e.ctx, e.col, recs); err != nil {
		e.t.Fatalf("Put: %v", err)
	}
}

func (e *env) get(ids ...string) map[string]store.Record {
	e.t.Helper()
	recs, err := e.d.Get(e.ctx, e.col, ids)
	if err != nil {
		e.t.Fatalf("Get: %v", err)
	}
	out := map[string]store.Record{}
	for _, r := range recs {
		out[r.ID] = r
	}
	return out
}

func (e *env) findAll(f store.Filter) []store.Record {
	e.t.Helper()
	recs, err := store.FindAll(e.ctx, e.d, e.col, f)
	if err != nil {
		e.t.Fatalf("Find: %v", err)
	}
	return recs
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// uuid makes a random version 4 UUID.
func uuid() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// record makes an element record with its fields filled.
func record(doc, parent, typ, text string) store.Record {
	id := uuid()
	node, _ := json.Marshal(map[string]any{
		"id": id, "scene": parent, "type": typ,
		"authors": []string{doc}, "text": map[string]string{"en": text},
	})
	return store.Record{
		ID: id, Doc: doc, Parent: parent, Kind: "element", Type: typ, Order: "V",
		Rev: 3, TextRev: 2, CRev: 1,
		Updated: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		Node:    node, Text: text,
	}
}

// same compares two records field by field, allowing for drivers that store
// time at millisecond precision and in another zone.
func same(t *testing.T, got, want store.Record) {
	t.Helper()
	if got.ID != want.ID || got.Doc != want.Doc || got.Parent != want.Parent ||
		got.Kind != want.Kind || got.Type != want.Type || got.Order != want.Order ||
		got.Rev != want.Rev || got.TextRev != want.TextRev || got.CRev != want.CRev ||
		got.Seq != want.Seq || got.Layout != want.Layout || got.Text != want.Text ||
		got.VectorModel != want.VectorModel {
		t.Errorf("record differs:\n got  %+v\n want %+v", brief(got), brief(want))
	}
	if !got.Updated.Truncate(time.Millisecond).Equal(want.Updated.Truncate(time.Millisecond)) {
		t.Errorf("Updated = %v, want %v", got.Updated, want.Updated)
	}
	if !jsonEqual(got.Node, want.Node) {
		t.Errorf("Node = %s, want %s", got.Node, want.Node)
	}
	if len(want.VectorMeta) > 0 || len(got.VectorMeta) > 0 {
		if !jsonEqual(got.VectorMeta, want.VectorMeta) {
			t.Errorf("VectorMeta = %s, want %s", got.VectorMeta, want.VectorMeta)
		}
	}
	if !slices.Equal(got.Vector, want.Vector) {
		t.Errorf("Vector = %v, want %v", got.Vector, want.Vector)
	}
}

// brief drops the bulky fields for error messages.
func brief(r store.Record) store.Record {
	r.Node, r.Vector, r.VectorMeta = nil, nil, nil
	if len(r.Text) > 40 {
		r.Text = r.Text[:40] + "…"
	}
	return r
}

func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	return string(ja) == string(jb)
}

func testEnsureTwice(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	if err := e.d.Ensure(e.ctx, e.specs()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	r := record(uuid(), uuid(), "action", "still works")
	e.put(r)
	if got := e.get(r.ID); len(got) != 1 {
		t.Fatalf("Get after second Ensure found %d", len(got))
	}
}

func testPutGet(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc, scene := uuid(), uuid()
	a := record(doc, scene, "action", "He runs.")
	b := record(doc, scene, "dialogue", "Get down!")
	root := store.Record{
		ID: doc, Doc: doc, Kind: "document", Rev: 1, TextRev: 1, CRev: 4, Seq: 17,
		Layout: "abc123", Updated: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Node: json.RawMessage(`{"id":"` + doc + `","title":{"en":"T"}}`), Text: "T",
	}
	e.put(a, b, root)

	got := e.get(a.ID, b.ID, root.ID, uuid())
	if len(got) != 3 {
		t.Fatalf("Get returned %d records, want 3 (a missing ID is skipped)", len(got))
	}
	same(t, got[a.ID], a)
	same(t, got[b.ID], b)
	same(t, got[root.ID], root)
}

func testUpsert(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	r := record(uuid(), uuid(), "action", "first")
	e.put(r)
	r2 := record(r.Doc, r.Parent, "general", "second")
	r2.ID = r.ID
	r2.Rev = 9
	e.put(r2)
	got := e.get(r.ID)
	same(t, got[r.ID], r2)
	if n := len(e.findAll(store.Filter{Doc: r.Doc})); n != 1 {
		t.Errorf("upsert left %d records", n)
	}
}

func testFindFilters(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc1, doc2, s1, s2 := uuid(), uuid(), uuid(), uuid()
	var all []store.Record
	for i := 0; i < 5; i++ {
		all = append(all, record(doc1, s1, "action", fmt.Sprint("a", i)))
	}
	for i := 0; i < 3; i++ {
		all = append(all, record(doc1, s2, "dialogue", fmt.Sprint("d", i)))
	}
	for i := 0; i < 4; i++ {
		all = append(all, record(doc2, uuid(), "action", fmt.Sprint("x", i)))
	}
	scene := record(doc1, doc1, "", "scene")
	scene.Kind = "scene"
	all = append(all, scene)
	e.put(all...)

	cases := []struct {
		name string
		f    store.Filter
		want int
	}{
		{"doc", store.Filter{Doc: doc1}, 9},
		{"parent", store.Filter{Parent: s1}, 5},
		{"kind", store.Filter{Doc: doc1, Kind: "element"}, 8},
		{"type", store.Filter{Type: "dialogue"}, 3},
		{"type in doc", store.Filter{Doc: doc2, Type: "action"}, 4},
		{"ids", store.Filter{IDs: []string{all[0].ID, all[7].ID, uuid()}}, 2},
		{"ids and doc", store.Filter{Doc: doc2, IDs: []string{all[0].ID, all[9].ID}}, 1},
		{"nothing", store.Filter{Doc: uuid()}, 0},
		{"all", store.Filter{}, 13},
	}
	for _, c := range cases {
		got := e.findAll(c.f)
		if len(got) != c.want {
			t.Errorf("%s: %d records, want %d", c.name, len(got), c.want)
		}
		for _, r := range got {
			if !c.f.Match(r) {
				t.Errorf("%s: %s does not match the filter", c.name, r.ID)
			}
		}
	}
}

func testFindPaginates(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc := uuid()
	const n = 2600
	want := map[string]bool{}
	batch := make([]store.Record, 0, 500)
	for i := 0; i < n; i++ {
		r := record(doc, uuid(), "action", fmt.Sprint("line ", i))
		want[r.ID] = true
		batch = append(batch, r)
		if len(batch) == cap(batch) {
			e.put(batch...)
			batch = batch[:0]
		}
	}
	e.put(batch...)

	seen := map[string]bool{}
	after, pages := "", 0
	for {
		recs, next, err := e.d.Find(e.ctx, e.col, store.Filter{Doc: doc}, after, 300)
		if err != nil {
			t.Fatalf("Find page %d: %v", pages, err)
		}
		if len(recs) > 300 {
			t.Fatalf("page of %d records, limit 300", len(recs))
		}
		for _, r := range recs {
			if seen[r.ID] {
				t.Fatalf("%s returned twice", r.ID)
			}
			seen[r.ID] = true
		}
		pages++
		if next == "" {
			break
		}
		if pages > n {
			t.Fatal("pagination does not end")
		}
		after = next
	}
	if len(seen) != n {
		t.Errorf("pages returned %d records, want %d", len(seen), n)
	}
	if pages < 9 {
		t.Errorf("%d pages for %d records at 300 a page", pages, n)
	}
}

func testDelete(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc, s1, s2 := uuid(), uuid(), uuid()
	a, b := record(doc, s1, "action", "a"), record(doc, s1, "action", "b")
	c, d := record(doc, s2, "action", "c"), record(doc, s2, "shot", "d")
	e.put(a, b, c, d)

	if err := e.d.Delete(e.ctx, e.col, []string{a.ID, uuid()}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := e.get(a.ID, b.ID); len(got) != 1 || got[b.ID].ID == "" {
		t.Errorf("after Delete: %v", got)
	}
	if err := e.d.DeleteWhere(e.ctx, e.col, store.Filter{Parent: s2, Type: "shot"}); err != nil {
		t.Fatalf("DeleteWhere: %v", err)
	}
	left := e.findAll(store.Filter{Doc: doc})
	ids := []string{}
	for _, r := range left {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	want := []string{b.ID, c.ID}
	sort.Strings(want)
	if !slices.Equal(ids, want) {
		t.Errorf("left %v, want %v", ids, want)
	}
}

func vectorRecord(doc string, v []float32) store.Record {
	r := record(doc, uuid(), "action", "vector")
	r.Vector, r.VectorModel = v, Model
	r.VectorMeta = json.RawMessage(`{"index":0,"embedding":{"id":"` + uuid() + `","model":"` + Model + `"}}`)
	return r
}

func testVectors(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc := uuid()
	with := vectorRecord(doc, []float32{0.5, -0.25, 1, 0.125})
	without := record(doc, uuid(), "action", "no vector")
	e.put(with, without)
	got := e.get(with.ID, without.ID)
	same(t, got[with.ID], with)
	same(t, got[without.ID], without)

	// A record can lose its vector.
	with.Vector, with.VectorModel, with.VectorMeta = nil, "", nil
	e.put(with)
	same(t, e.get(with.ID)[with.ID], with)
}

func testSearchText(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc := uuid()
	gun := record(doc, uuid(), "action", "He pulls a gun from the drawer.")
	e.put(gun, record(doc, uuid(), "action", "She makes tea."), record(uuid(), uuid(), "action", "Another gun elsewhere."))

	hits, err := e.d.SearchText(e.ctx, e.col, "gun", store.Filter{Doc: doc}, 10)
	if !e.d.Caps().FullText {
		if !errors.Is(err, store.ErrUnsupported) {
			t.Fatalf("no full text in caps, but SearchText returned %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(hits) != 1 || hits[0].Record.ID != gun.ID {
		t.Fatalf("hits = %v, want only %s", hitIDs(hits), gun.ID)
	}
}

func testSearchVector(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc := uuid()
	near := vectorRecord(doc, []float32{1, 0, 0, 0})
	mid := vectorRecord(doc, []float32{1, 1, 0, 0})
	far := vectorRecord(doc, []float32{0, 0, 1, 0})
	e.put(near, mid, far, record(doc, uuid(), "action", "no vector"))

	hits, err := e.d.SearchVector(e.ctx, e.col, Model, []float32{0.9, 0.1, 0, 0}, store.Filter{Doc: doc}, 2)
	if !e.d.Caps().Vector {
		if !errors.Is(err, store.ErrUnsupported) {
			t.Fatalf("no vectors in caps, but SearchVector returned %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(hits) != 2 || hits[0].Record.ID != near.ID || hits[1].Record.ID != mid.ID {
		t.Fatalf("hits = %v, want %s then %s", hitIDs(hits), near.ID, mid.ID)
	}
}

func hitIDs(hits []store.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Record.ID
	}
	return out
}

func testUnicode(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	doc := uuid()
	texts := []string{
		"Emoji 🎬🔫 and a ZWJ family 👨‍👩‍👧",
		"مرحبا بالعالم — right to left",
		strings.Repeat("ab", 5000),
		"Quotes \" and \\ backslashes, <tags> & ampersands",
	}
	var recs []store.Record
	for _, s := range texts {
		recs = append(recs, record(doc, uuid(), "action", s))
	}
	e.put(recs...)
	got := e.get(ids(recs)...)
	for _, r := range recs {
		same(t, got[r.ID], r)
	}
}

func ids(recs []store.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func testTx(t *testing.T, newDriver Factory) {
	e := setup(t, newDriver)
	r := record(uuid(), uuid(), "action", "in a transaction")
	err := e.d.Tx(e.ctx, func(ctx context.Context) error {
		return e.d.Put(ctx, e.col, []store.Record{r})
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if len(e.get(r.ID)) != 1 {
		t.Fatal("write inside Tx was lost")
	}
	if !e.d.Caps().Transactions {
		return
	}
	boom := errors.New("boom")
	r2 := record(r.Doc, uuid(), "action", "rolled back")
	err = e.d.Tx(e.ctx, func(ctx context.Context) error {
		if err := e.d.Put(ctx, e.col, []store.Record{r2}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx returned %v, want the function's error", err)
	}
	if len(e.get(r2.ID)) != 0 {
		t.Error("write inside a failed Tx was kept")
	}
}
