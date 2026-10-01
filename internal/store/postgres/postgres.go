// Package postgres stores records in PostgreSQL tables (R-DRV-03): one table
// per collection, a generated tsvector with a GIN index for full-text search,
// and a pgvector column for levels that declare a native vector, when the
// extension is available.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Driver is a PostgreSQL database.
type Driver struct {
	pool     *pgxpool.Pool
	tx       bool
	pgvector bool
	mu       sync.RWMutex
	vectors  map[string]int // collections with a vec column, and its size
}

// Open connects and checks for pgvector (R-DRV-03). transactions turns on Tx.
func Open(ctx context.Context, url string, transactions bool) (*Driver, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	d := &Driver{pool: pool, tx: transactions, vectors: map[string]int{}}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err == nil {
		d.pgvector = true
	}
	return d, nil
}

// Name implements store.Driver.
func (d *Driver) Name() string { return "postgres" }

// Caps implements store.Driver.
func (d *Driver) Caps() store.Caps {
	return store.Caps{Transactions: d.tx, FullText: true, Vector: d.pgvector, NativeWholeDoc: true}
}

// HasPgvector reports whether the vector extension is installed.
func (d *Driver) HasPgvector() bool { return d.pgvector }

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

// Ensure implements store.Driver.
func (d *Driver) Ensure(ctx context.Context, cols []store.CollectionSpec) error {
	for _, c := range cols {
		t := ident(c.Name)
		vec := ""
		if c.Vector != nil && d.pgvector {
			vec = fmt.Sprintf(", vec vector(%d)", c.Vector.Dimensions)
		}
		stmts := []string{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				id uuid PRIMARY KEY, doc uuid, parent uuid, kind text NOT NULL DEFAULT '', type text NOT NULL DEFAULT '',
				ord text NOT NULL DEFAULT '', rev bigint NOT NULL DEFAULT 0, text_rev bigint NOT NULL DEFAULT 0,
				crev bigint NOT NULL DEFAULT 0, seq bigint NOT NULL DEFAULT 0, updated timestamptz, layout text NOT NULL DEFAULT '',
				node jsonb, text text NOT NULL DEFAULT '', vector_model text NOT NULL DEFAULT '', vector_meta jsonb,
				vector_raw real[]%s,
				tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', coalesce(text, ''))) STORED)`, t, vec),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (doc, parent)`, ident(c.Name+"_doc_parent"), t),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s USING gin (tsv)`, ident(c.Name+"_tsv"), t),
		}
		for _, s := range stmts {
			if _, err := d.pool.Exec(ctx, s); err != nil {
				return fmt.Errorf("postgres: ensure %s: %w", c.Name, err)
			}
		}
		if vec != "" {
			d.mu.Lock()
			d.vectors[c.Name] = c.Vector.Dimensions
			d.mu.Unlock()
		}
	}
	return nil
}

type txKey struct{}

// querier is the transaction in ctx, or the pool.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

func (d *Driver) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return d.pool
}

// Tx implements store.Driver.
func (d *Driver) Tx(ctx context.Context, fn func(ctx context.Context) error) error {
	if !d.tx {
		return fn(ctx)
	}
	if _, nested := ctx.Value(txKey{}).(pgx.Tx); nested {
		return fn(ctx)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func vecText(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func rawJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// Put implements store.Driver: an upsert per record, sent as one batch.
func (d *Driver) Put(ctx context.Context, col string, recs []store.Record) error {
	if len(recs) == 0 {
		return nil
	}
	d.mu.RLock()
	_, hasVec := d.vectors[col]
	d.mu.RUnlock()
	t := ident(col)
	cols := `id, doc, parent, kind, type, ord, rev, text_rev, crev, seq, updated, layout, node, text, vector_model, vector_meta, vector_raw`
	vals := `$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16::jsonb, $17`
	set := `doc = EXCLUDED.doc, parent = EXCLUDED.parent, kind = EXCLUDED.kind, type = EXCLUDED.type, ord = EXCLUDED.ord,
		rev = EXCLUDED.rev, text_rev = EXCLUDED.text_rev, crev = EXCLUDED.crev, seq = EXCLUDED.seq, updated = EXCLUDED.updated,
		layout = EXCLUDED.layout, node = EXCLUDED.node, text = EXCLUDED.text, vector_model = EXCLUDED.vector_model,
		vector_meta = EXCLUDED.vector_meta, vector_raw = EXCLUDED.vector_raw`
	if hasVec {
		cols += ", vec"
		vals += ", $18::vector"
		set += ", vec = EXCLUDED.vec"
	}
	sql := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO UPDATE SET %s`, t, cols, vals, set)
	b := &pgx.Batch{}
	for _, r := range recs {
		var updated any
		if !r.Updated.IsZero() {
			updated = r.Updated
		}
		var raw any
		if r.Vector != nil {
			raw = r.Vector
		}
		args := []any{r.ID, nullUUID(r.Doc), nullUUID(r.Parent), r.Kind, r.Type, r.Order, int64(r.Rev), int64(r.TextRev),
			int64(r.CRev), int64(r.Seq), updated, r.Layout, rawJSON(r.Node), r.Text, r.VectorModel, rawJSON(r.VectorMeta), raw}
		if hasVec {
			var v any
			if r.Vector != nil {
				v = vecText(r.Vector)
			}
			args = append(args, v)
		}
		b.Queue(sql, args...)
	}
	br := d.q(ctx).SendBatch(ctx, b)
	defer br.Close()
	for range recs {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("postgres: put into %s: %w", col, err)
		}
	}
	return nil
}

const selectCols = `id::text, coalesce(doc::text, ''), coalesce(parent::text, ''), kind, type, ord, rev, text_rev, crev, seq,
	updated, layout, coalesce(node::text, ''), text, vector_model, coalesce(vector_meta::text, ''), vector_raw`

func scan(rows pgx.Rows, extra ...any) (store.Record, error) {
	var r store.Record
	var rev, textRev, crev, seq int64
	var updated *time.Time
	var node, meta string
	var raw []float32
	dest := []any{&r.ID, &r.Doc, &r.Parent, &r.Kind, &r.Type, &r.Order, &rev, &textRev, &crev, &seq, &updated, &r.Layout,
		&node, &r.Text, &r.VectorModel, &meta, &raw}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return r, err
	}
	r.Rev, r.TextRev, r.CRev, r.Seq = uint64(rev), uint64(textRev), uint64(crev), uint64(seq)
	if updated != nil {
		r.Updated = updated.UTC()
	}
	if node != "" {
		r.Node = json.RawMessage(node)
	}
	if meta != "" {
		r.VectorMeta = json.RawMessage(meta)
	}
	if len(raw) > 0 {
		r.Vector = raw
	}
	return r, nil
}

func collect(rows pgx.Rows, err error) ([]store.Record, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get implements store.Driver.
func (d *Driver) Get(ctx context.Context, col string, ids []string) ([]store.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	recs, err := collect(d.q(ctx).Query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ANY($1::uuid[])`, selectCols, ident(col)), ids))
	if err != nil {
		return nil, fmt.Errorf("postgres: get from %s: %w", col, err)
	}
	return recs, nil
}

// where turns a filter into SQL conditions and arguments, numbered from n.
func where(f store.Filter, n int) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		n++
		conds = append(conds, fmt.Sprintf(cond, n))
		args = append(args, v)
	}
	if f.Doc != "" {
		add("doc = $%d::uuid", f.Doc)
	}
	if f.Parent != "" {
		add("parent = $%d::uuid", f.Parent)
	}
	if f.Kind != "" {
		add("kind = $%d", f.Kind)
	}
	if f.Type != "" {
		add("type = $%d", f.Type)
	}
	if len(f.IDs) > 0 {
		add("id = ANY($%d::uuid[])", f.IDs)
	}
	if len(conds) == 0 {
		return "TRUE", nil
	}
	return strings.Join(conds, " AND "), args
}

// Find implements store.Driver, paging by ID.
func (d *Driver) Find(ctx context.Context, col string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("postgres: limit must be positive")
	}
	cond, args := where(f, 0)
	if after != "" {
		args = append(args, after)
		cond += fmt.Sprintf(" AND id > $%d::uuid", len(args))
	}
	args = append(args, limit+1)
	sql := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY id LIMIT $%d`, selectCols, ident(col), cond, len(args))
	recs, err := collect(d.q(ctx).Query(ctx, sql, args...))
	if err != nil {
		return nil, "", fmt.Errorf("postgres: find in %s: %w", col, err)
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		next = recs[limit-1].ID
	}
	return recs, next, nil
}

// Delete implements store.Driver.
func (d *Driver) Delete(ctx context.Context, col string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := d.q(ctx).Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1::uuid[])`, ident(col)), ids); err != nil {
		return fmt.Errorf("postgres: delete from %s: %w", col, err)
	}
	return nil
}

// DeleteWhere implements store.Driver.
func (d *Driver) DeleteWhere(ctx context.Context, col string, f store.Filter) error {
	cond, args := where(f, 0)
	if _, err := d.q(ctx).Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s`, ident(col), cond), args...); err != nil {
		return fmt.Errorf("postgres: delete from %s: %w", col, err)
	}
	return nil
}

func hits(rows pgx.Rows, err error) ([]store.Hit, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Hit
	for rows.Next() {
		var score float64
		r, err := scan(rows, &score)
		if err != nil {
			return nil, err
		}
		out = append(out, store.Hit{Record: r, Score: score})
	}
	return out, rows.Err()
}

// SearchText implements store.Driver with the tsvector index.
func (d *Driver) SearchText(ctx context.Context, col, q string, f store.Filter, k int) ([]store.Hit, error) {
	cond, args := where(f, 1)
	args = append([]any{q}, args...)
	args = append(args, k)
	sql := fmt.Sprintf(`SELECT %s, ts_rank(tsv, plainto_tsquery('simple', $1)) AS score FROM %s
		WHERE tsv @@ plainto_tsquery('simple', $1) AND %s ORDER BY score DESC, id LIMIT $%d`, selectCols, ident(col), cond, len(args))
	out, err := hits(d.q(ctx).Query(ctx, sql, args...))
	if err != nil {
		return nil, fmt.Errorf("postgres: text search in %s: %w", col, err)
	}
	return out, nil
}

// SearchVector implements store.Driver with pgvector's cosine distance.
func (d *Driver) SearchVector(ctx context.Context, col, model string, v []float32, f store.Filter, k int) ([]store.Hit, error) {
	d.mu.RLock()
	_, hasVec := d.vectors[col]
	d.mu.RUnlock()
	if !hasVec {
		return nil, store.ErrUnsupported
	}
	cond, args := where(f, 2)
	args = append([]any{vecText(v), model}, args...)
	args = append(args, k)
	sql := fmt.Sprintf(`SELECT %s, 1 - (vec <=> $1::vector) AS score FROM %s
		WHERE vec IS NOT NULL AND vector_model = $2 AND %s ORDER BY vec <=> $1::vector, id LIMIT $%d`, selectCols, ident(col), cond, len(args))
	out, err := hits(d.q(ctx).Query(ctx, sql, args...))
	if err != nil {
		return nil, fmt.Errorf("postgres: vector search in %s: %w", col, err)
	}
	return out, nil
}

// Ping implements store.Driver.
func (d *Driver) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Close implements store.Driver.
func (d *Driver) Close() error {
	d.pool.Close()
	return nil
}
