package docs

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/layout"
	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
	"github.com/screenjson/screenjson-db-importer/internal/state"
	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Options configures a Manager.
type Options struct {
	Driver      store.Driver
	Layout      layout.Layout
	Fingerprint string
	State       *state.Store
	Schema      *schema.Set
	Order       *schema.Order
	Paths       *paths.Table
	Publisher   Publisher
	Hooks       Hooks
	Log         *slog.Logger

	// MaxDocuments and MaxBytes bound the cache (R-ARCH-09).
	MaxDocuments int
	MaxBytes     int64
	// QueueLength bounds each document's write queue (R-ARCH-04).
	QueueLength int
	// DefaultTTL and MaxTTL bound checkouts (R-CO-01).
	DefaultTTL, MaxTTL time.Duration
	// Numbering is numbering.default (R-DER-03).
	Numbering string
	// Transactions wraps multi-record writes in a driver transaction.
	Transactions bool
	// RequireIfMatch refuses writes without If-Match (428).
	RequireIfMatch bool
	// OnRepair counts a load-time repair (screenjson_repairs_total).
	OnRepair func(code string)
	// OnCache counts a cache hit or miss.
	OnCache func(hit bool)
	// Now is the clock.
	Now func() time.Time
}

// Manager owns every open document.
type Manager struct {
	o   Options
	ids uuidGen

	mu      sync.Mutex
	docs    map[string]*doc
	lru     *list.List // of *doc, most recently used at the front
	loading map[string]*loadCall
	bytes   int64

	co     *checkouts
	passes *passSet
	// gone remembers deleted documents so they answer 410 (R-PATH-08).
	gone *docSet
	// creating serialises imports of the same document ID.
	creating keyedMutex

	stop    chan struct{}
	stopped chan struct{}
}

// doc is one cached document.
type doc struct {
	id string
	// tree is the committed tree. Readers load it without locking; the write
	// queue replaces it once a write is committed (R-ARCH-07).
	tree atomic.Pointer[model.Tree]
	// queue is the document's write queue (R-ARCH-02).
	queue chan *op
	// pending counts operations queued or running. A document with pending
	// work is never evicted, and its queue is never closed under a sender.
	pending atomic.Int64
	bytes   int64
	elem    *list.Element
	// lastWrite is when the document was last written, in Unix nanoseconds.
	lastWrite atomic.Int64
	// deleted is set once the document has been deleted. Its queue then
	// answers anything still queued with 410 and stops.
	deleted atomic.Bool
}

type loadCall struct {
	done chan struct{}
	d    *doc
	err  error
}

// op is one write waiting in a queue.
type op struct {
	ctx  context.Context
	req  *Request
	fn   func(w *write) (replyFunc, error)
	resp chan opResult
}

type opResult struct {
	resp *Response
	err  error
	// later is closed once the write's deferred work is done.
	later chan struct{}
}

// New makes a manager. Call Close when done.
func New(o Options) (*Manager, error) {
	if o.Driver == nil || o.State == nil || o.Schema == nil || o.Order == nil || o.Paths == nil {
		return nil, errors.New("docs: driver, state, schema, order and paths are required")
	}
	if err := o.Layout.Validate(); err != nil {
		return nil, err
	}
	if o.Publisher == nil {
		o.Publisher = nopPublisher{}
	}
	if o.Hooks == nil {
		o.Hooks = nopHooks{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.MaxDocuments <= 0 {
		o.MaxDocuments = 256
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 1 << 30
	}
	if o.QueueLength <= 0 {
		o.QueueLength = 1000
	}
	if o.DefaultTTL <= 0 {
		o.DefaultTTL = 300 * time.Second
	}
	if o.MaxTTL < o.DefaultTTL {
		o.MaxTTL = 3600 * time.Second
	}
	if o.Numbering == "" {
		o.Numbering = "none"
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.OnRepair == nil {
		o.OnRepair = func(string) {}
	}
	if o.OnCache == nil {
		o.OnCache = func(bool) {}
	}
	m := &Manager{
		o: o, docs: map[string]*doc{}, lru: list.New(), loading: map[string]*loadCall{},
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
	m.gone = &docSet{ids: map[string]struct{}{}}
	m.co = newCheckouts(m)
	m.passes = newPassSet(o.State)
	go m.janitor()
	return m, nil
}

// Close stops the manager. Queued writes finish first.
func (m *Manager) Close() {
	close(m.stop)
	<-m.stopped
	m.mu.Lock()
	docs := make([]*doc, 0, len(m.docs))
	for _, d := range m.docs {
		docs = append(docs, d)
	}
	m.docs = map[string]*doc{}
	m.mu.Unlock()
	for _, d := range docs {
		close(d.queue)
	}
}

// janitor expires checkouts once a second, publishing checkout.changed for
// each (R-CO-04: nobody has to act for them to go).
func (m *Manager) janitor() {
	defer close(m.stopped)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.co.expire()
		}
	}
}

// Stats is a snapshot for /status and metrics.
type Stats struct {
	OpenDocuments int
	CacheBytes    int64
	Checkouts     int
	QueueDepths   map[string]int64
}

// Stats reports what the manager holds.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	s := Stats{OpenDocuments: len(m.docs), CacheBytes: m.bytes, QueueDepths: map[string]int64{}}
	for id, d := range m.docs {
		if n := d.pending.Load(); n > 0 {
			s.QueueDepths[id] = n
		}
	}
	m.mu.Unlock()
	s.Checkouts = m.co.count()
	return s
}

// acquire returns a cached document, loading it once however many callers ask
// at the same time (R-ARCH-08). With forWrite, the document's pending count is
// raised before the cache lock is released, so it can't be evicted before the
// caller's operation is queued; the caller must lower it again.
func (m *Manager) acquire(ctx context.Context, id string, forWrite bool) (*doc, error) {
	for {
		m.mu.Lock()
		if d, ok := m.docs[id]; ok {
			m.lru.MoveToFront(d.elem)
			if forWrite {
				d.pending.Add(1)
			}
			m.mu.Unlock()
			m.o.OnCache(true)
			return d, nil
		}
		if lc, ok := m.loading[id]; ok {
			m.mu.Unlock()
			select {
			case <-lc.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if lc.err != nil {
				return nil, lc.err
			}
			continue
		}
		lc := &loadCall{done: make(chan struct{})}
		m.loading[id] = lc
		m.mu.Unlock()
		m.o.OnCache(false)

		d, err := m.load(ctx, id)
		m.mu.Lock()
		delete(m.loading, id)
		lc.d, lc.err = d, err
		if err == nil {
			m.insert(d)
		}
		m.mu.Unlock()
		close(lc.done)
		if err != nil {
			return nil, err
		}
	}
}

// load reads a document from the driver, writes back any repairs the load made
// (R-ARCH-10), and starts its write queue.
func (m *Manager) load(ctx context.Context, id string) (*doc, error) {
	t, repairs, err := m.o.Layout.Load(ctx, m.o.Driver, id)
	if errors.Is(err, layout.ErrNotFound) {
		if t, ok := m.recentlyDeleted(id); ok {
			return nil, t
		}
		return nil, notFound("no document %s", id)
	}
	if err != nil {
		return nil, storageErr(fmt.Errorf("load %s: %w", id, err))
	}
	fillEmbeddedEnvelopes(t)
	// A stale cast is repaired on load too (R-ARCH-10).
	for _, sc := range t.Scenes() {
		if cast, stale := staleCast(sc); stale {
			sc.Fields["cast"] = cast
			repairs = append(repairs, layout.Repair{Code: RepairStaleCast, Collection: m.o.Layout.Collection(model.KindScene),
				ID: sc.ID, Message: "cast did not match the scene's body"})
		}
	}
	if len(repairs) > 0 {
		if err := m.writeRepairs(ctx, t, repairs); err != nil {
			return nil, storageErr(err)
		}
	}
	d := &doc{id: id, queue: make(chan *op, m.o.QueueLength)}
	d.tree.Store(t)
	if raw, err := t.Marshal(); err == nil {
		d.bytes = int64(len(raw))
	}
	go m.run(d)
	return d, nil
}

// RepairStaleCast is the repair code for a cast recomputed on load.
const RepairStaleCast = "stale_cast"

// recentlyDeleted answers 410 for a document deleted while the server has been
// running. Deleted documents are remembered only in memory, like deleted nodes.
func (m *Manager) recentlyDeleted(id string) (*Error, bool) {
	if m.deletedDocs().has(id) {
		return newErr(http.StatusGone, CodeGone, "document %s was deleted", id), true
	}
	return nil, false
}

// fillEmbeddedEnvelopes gives embedded objects that have no envelope yet the
// same starting point layout gives embedded tree nodes (D-008 point 4).
func fillEmbeddedEnvelopes(t *model.Tree) {
	seq := t.Seq()
	if seq == 0 {
		seq = 1
	}
	t.Each(func(n *model.Node) {
		if n.Env.Rev == 0 {
			n.Env.Rev, n.Env.TextRev, n.Env.CRev = seq, seq, seq
		}
	})
}

// writeRepairs logs each repair and writes the fix back.
func (m *Manager) writeRepairs(ctx context.Context, t *model.Tree, repairs []layout.Repair) error {
	var puts []layout.Put
	seen := map[string]bool{}
	for _, r := range repairs {
		m.o.Log.Warn("repaired document on load", "doc", t.Doc(), "code", r.Code, "record", r.ID,
			"collection", r.Collection, "detail", r.Message)
		m.o.OnRepair(r.Code)
		if r.Delete {
			if err := m.o.Driver.Delete(ctx, r.Collection, []string{r.ID}); err != nil {
				return fmt.Errorf("repair %s: %w", r.ID, err)
			}
			continue
		}
		n := t.Node(r.ID)
		if n == nil || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		p, err := m.o.Layout.Record(t, m.o.Layout.Owner(n))
		if err != nil {
			return err
		}
		puts = append(puts, p)
	}
	return layout.WritePuts(ctx, m.o.Driver, puts)
}

// insert adds a loaded document to the cache and evicts others past the
// bounds. Callers hold m.mu.
func (m *Manager) insert(d *doc) {
	d.elem = m.lru.PushFront(d)
	m.docs[d.id] = d
	m.bytes += d.bytes
	m.evict()
}

// evict drops least recently used documents while the cache is over either
// bound, skipping any with queued writes, active checkouts or live subscribers
// (R-ARCH-09). Callers hold m.mu.
func (m *Manager) evict() {
	for e := m.lru.Back(); e != nil && (len(m.docs) > m.o.MaxDocuments || m.bytes > m.o.MaxBytes); {
		d := e.Value.(*doc)
		prev := e.Prev()
		if d.pending.Load() == 0 && !m.co.has(d.id) && !m.o.Publisher.Subscribed(d.id) {
			m.drop(d)
		}
		e = prev
	}
}

// drop removes a document from the cache. Callers hold m.mu and have checked
// it has no pending work.
func (m *Manager) drop(d *doc) {
	m.lru.Remove(d.elem)
	delete(m.docs, d.id)
	m.bytes -= d.bytes
	close(d.queue)
}

// Evict drops every idle document from the cache. It is the --test-evict hook
// of SPEC.md 17.3 and is also used by relayout.
func (m *Manager) Evict() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, d := range m.docs {
		if d.pending.Load() == 0 {
			m.drop(d)
			n++
		}
	}
	return n
}

// forget removes a deleted document from the cache. Its queue goroutine keeps
// running until the operations already counted in pending are answered.
func (m *Manager) forget(d *doc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.deleted.Store(true)
	if cur, ok := m.docs[d.id]; ok && cur == d {
		m.lru.Remove(d.elem)
		delete(m.docs, d.id)
		m.bytes -= d.bytes
	}
}

// Written reports when each cached document was last written, for Git auto
// commit. Documents not written since they were loaded are left out.
func (m *Manager) Written() map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]time.Time{}
	for id, d := range m.docs {
		if ns := d.lastWrite.Load(); ns > 0 {
			out[id] = time.Unix(0, ns)
		}
	}
	return out
}

// Tree returns the committed tree of a document for reading.
func (m *Manager) Tree(ctx context.Context, id string) (*model.Tree, error) {
	d, err := m.acquire(ctx, id, false)
	if err != nil {
		return nil, err
	}
	return d.tree.Load(), nil
}

// submit queues a write on a document and waits for it. A full queue answers
// 429 with Retry-After (R-ARCH-04).
func (m *Manager) submit(ctx context.Context, id string, req *Request, fn func(w *write) (replyFunc, error)) (*Response, error) {
	d, err := m.acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	o := &op{ctx: ctx, req: req, fn: fn, resp: make(chan opResult, 1)}
	select {
	case d.queue <- o:
	default:
		d.pending.Add(-1)
		e := newErr(http.StatusTooManyRequests, CodeBusy, "the write queue for document %s is full", id)
		e.Header = map[string]string{"Retry-After": "1"}
		return nil, e
	}
	select {
	case r := <-o.resp:
		if r.later != nil {
			<-r.later
		}
		return r.resp, r.err
	case <-ctx.Done():
		// The write still runs; the caller has gone.
		return nil, ctx.Err()
	}
}

// run is a document's write queue: one goroutine applying operations in
// arrival order (R-ARCH-02).
func (m *Manager) run(d *doc) {
	for o := range d.queue {
		if d.deleted.Load() {
			o.resp <- opResult{err: newErr(http.StatusGone, CodeGone, "document %s was deleted", d.id)}
		} else {
			resp, later, err := m.apply(d, o)
			r := opResult{resp: resp, err: err}
			if len(later) > 0 {
				// Run it off the queue; it finishes even if the caller has gone.
				r.later = make(chan struct{})
				go func() {
					defer close(r.later)
					for _, f := range later {
						f()
					}
				}()
			}
			o.resp <- r
		}
		if d.pending.Add(-1) == 0 && d.deleted.Load() {
			// Deleted and nothing left: nobody can reach this queue any more.
			return
		}
	}
}

// apply runs one operation, turning a panic into a 500 so it can't take down
// the queue.
func (m *Manager) apply(d *doc, o *op) (resp *Response, later []func(), err error) {
	defer func() {
		if r := recover(); r != nil {
			m.o.Log.Error("panic in write", "doc", d.id, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			resp, later, err = nil, nil, newErr(http.StatusInternalServerError, "internal", "internal error")
		}
	}()
	w := m.newWrite(d, o)
	reply, err := o.fn(w)
	if err != nil {
		return nil, nil, err
	}
	if err := w.commit(o.ctx); err != nil {
		return nil, nil, err
	}
	w.finish()
	resp = reply()
	if resp.Seq == 0 && w.t != nil {
		resp.Seq = w.t.Seq()
	}
	return resp, w.later, nil
}

func storageErr(err error) *Error {
	e := newErr(http.StatusServiceUnavailable, CodeStorage, "storage error: %v", err)
	return e
}

// deletedDocs is the in-memory record of deleted documents.
func (m *Manager) deletedDocs() *docSet { return m.gone }

// docSet is a bounded set of deleted document IDs.
type docSet struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
}

func (s *docSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}

func (s *docSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	for len(s.order) > model.MaxDeletedTracked {
		delete(s.ids, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *docSet) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

// marshalNode encodes a value for a response or an event.
func marshalNode(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
