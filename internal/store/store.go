// Package store is the contract every database driver meets (SPEC.md 5.6). A
// driver stores records and returns them by ID or by filter; order, locks,
// revisions and checks all live in the server (R-ARCH-01).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// ErrUnsupported is returned by a driver asked for something its Caps say it
// can't do, such as vector search on a driver without vectors.
var ErrUnsupported = errors.New("store: not supported by this driver")

// Record is one stored row, document or object.
type Record struct {
	// ID is the node's UUID and the primary key. The analysis record, which
	// has no UUID of its own, uses the document's.
	ID string
	// Doc is the document's UUID.
	Doc string
	// Parent is the parent node's UUID; the document's for scenes, characters
	// and analysis. Empty for the root.
	Parent string
	// Kind is the node kind.
	Kind string
	// Type is the element type, or empty.
	Type string
	// Order is the key sorting the record among its siblings. Empty for the
	// root and analysis.
	Order string
	// Rev, TextRev and CRev are the node's revision numbers.
	Rev, TextRev, CRev uint64
	// Seq is the document's sequence number. Root only.
	Seq uint64
	// Updated is when the node was last written.
	Updated time.Time
	// Layout is the layout fingerprint the document was written with. Root only.
	Layout string
	// Node is the node's ScreenJSON with every separately stored child removed.
	Node json.RawMessage
	// Text is the plain text to search on, in the document's primary language.
	Text string
	// Vector is the native vector, or nil.
	Vector []float32
	// VectorModel names the model Vector came from.
	VectorModel string
	// VectorMeta holds the rest of the native embedding — everything but its
	// values — and its place in the node's embedding list, so that reads can
	// rebuild analysis.embeddings exactly (R-STORE-13). It is envelope data and
	// never part of Node. See docs/DECISIONS.md D-008.
	VectorMeta json.RawMessage
}

// Clone returns a deep copy, so a driver holding records in memory never
// shares slices with its callers.
func (r Record) Clone() Record {
	out := r
	out.Node = slices.Clone(r.Node)
	out.Vector = slices.Clone(r.Vector)
	out.VectorMeta = slices.Clone(r.VectorMeta)
	return out
}

// Filter selects records. Empty fields match anything.
type Filter struct {
	Doc, Parent, Kind, Type string
	// IDs, when not empty, limits the match to these IDs.
	IDs []string
}

// Match reports whether a record passes the filter. Drivers that can't push a
// filter down to the database use it to filter after loading.
func (f Filter) Match(r Record) bool {
	if f.Doc != "" && r.Doc != f.Doc {
		return false
	}
	if f.Parent != "" && r.Parent != f.Parent {
		return false
	}
	if f.Kind != "" && r.Kind != f.Kind {
		return false
	}
	if f.Type != "" && r.Type != f.Type {
		return false
	}
	if len(f.IDs) > 0 && !slices.Contains(f.IDs, r.ID) {
		return false
	}
	return true
}

// Caps are what a driver can do.
type Caps struct {
	// Transactions means Tx makes its writes atomic.
	Transactions bool `json:"transactions"`
	// FullText means SearchText works.
	FullText bool `json:"full_text"`
	// Vector means SearchVector works on native vectors.
	Vector bool `json:"vector"`
	// NativeWholeDoc means a whole document fits comfortably in one record.
	NativeWholeDoc bool `json:"native_whole_doc"`
	// MaxRecordBytes is the largest record the database accepts; 0 means no
	// practical limit.
	MaxRecordBytes int `json:"max_record_bytes"`
}

// VectorSpec is a level's native vector (SPEC.md 5.5).
type VectorSpec struct {
	Model      string `json:"model" yaml:"model"`
	Dimensions int    `json:"dimensions" yaml:"dimensions"`
}

// CollectionSpec describes one collection for Ensure.
type CollectionSpec struct {
	// Name is the collection, table, index or class name.
	Name string
	// Level is the layout level it holds: root, scenes, elements, characters,
	// analysis, or "system" for the server's own records such as the lease.
	Level string
	// Vector is the native vector, or nil.
	Vector *VectorSpec
	// FullText asks for a text index on Record.Text.
	FullText bool
}

// Hit is one search result.
type Hit struct {
	Record Record
	// Score is higher for better matches. Its scale depends on the driver.
	Score float64
}

// Driver is a database the server stores records in.
type Driver interface {
	// Name is the driver's config name, such as "memory" or "mongo".
	Name() string
	// Caps reports what the driver can do.
	Caps() Caps
	// Ensure creates collections, indexes and classes. It is safe to repeat.
	Ensure(ctx context.Context, cols []CollectionSpec) error
	// Put upserts records by ID.
	Put(ctx context.Context, col string, recs []Record) error
	// Get returns the records that exist among ids, in no particular order.
	Get(ctx context.Context, col string, ids []string) ([]Record, error)
	// Find returns up to limit records matching f, starting after the cursor
	// after ("" for the first page). next is "" on the last page. Records come
	// in no order the caller may rely on; the server sorts (R-STORE-10).
	Find(ctx context.Context, col string, f Filter, after string, limit int) (recs []Record, next string, err error)
	// Delete removes records by ID. Missing IDs are not an error.
	Delete(ctx context.Context, col string, ids []string) error
	// DeleteWhere removes every record matching f.
	DeleteWhere(ctx context.Context, col string, f Filter) error
	// Tx runs fn in a transaction, or directly when the driver has none.
	Tx(ctx context.Context, fn func(ctx context.Context) error) error
	// SearchText runs a full-text query, or returns ErrUnsupported.
	SearchText(ctx context.Context, col, q string, f Filter, k int) ([]Hit, error)
	// SearchVector runs a nearest-neighbour query on native vectors of model,
	// or returns ErrUnsupported.
	SearchVector(ctx context.Context, col, model string, v []float32, f Filter, k int) ([]Hit, error)
	// Ping checks the database is reachable.
	Ping(ctx context.Context) error
	// Close releases the driver's connections.
	Close() error
}

// FindAll pages through Find until the end.
func FindAll(ctx context.Context, d Driver, col string, f Filter) ([]Record, error) {
	var all []Record
	after := ""
	for {
		recs, next, err := d.Find(ctx, col, f, after, 1000)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
		if next == "" {
			return all, nil
		}
		after = next
	}
}
