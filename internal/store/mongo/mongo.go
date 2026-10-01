// Package mongo stores records in MongoDB collections (R-DRV-02) as
// {_id, <envelope>: {...}, node: {...}, text, vector?}: the ScreenJSON as a
// real document, the envelope beside it. Indexes on the envelope's doc and
// parent; a text index on text. Transactions are used only on a replica set,
// which is detected at startup. Mongo has no vector search here: SearchVector
// is unsupported (SPEC.md 17.7).
package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Driver is a MongoDB database.
type Driver struct {
	client     *mongo.Client
	db         *mongo.Database
	env        string
	replicaSet bool
	tx         bool
}

// Open connects, and detects whether the server is a replica set.
func Open(ctx context.Context, url, database, envelope string, transactions bool) (*Driver, error) {
	if database == "" {
		database = "screenjson"
	}
	if envelope == "" {
		envelope = "sj"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(url))
	if err != nil {
		return nil, fmt.Errorf("mongo: %w", err)
	}
	d := &Driver{client: client, db: client.Database(database), env: envelope}
	var hello bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("mongo: %w", err)
	}
	_, d.replicaSet = hello["setName"]
	d.tx = transactions && d.replicaSet
	return d, nil
}

// Name implements store.Driver.
func (d *Driver) Name() string { return "mongo" }

// Caps implements store.Driver.
func (d *Driver) Caps() store.Caps {
	return store.Caps{Transactions: d.tx, FullText: true, NativeWholeDoc: true, MaxRecordBytes: 16 << 20}
}

// ReplicaSet reports whether the server is a replica set.
func (d *Driver) ReplicaSet() bool { return d.replicaSet }

func (d *Driver) f(field string) string { return d.env + "." + field }

// Ensure implements store.Driver.
func (d *Driver) Ensure(ctx context.Context, cols []store.CollectionSpec) error {
	for _, c := range cols {
		coll := d.db.Collection(c.Name)
		models := []mongo.IndexModel{
			{Keys: bson.D{{Key: d.f("doc"), Value: 1}, {Key: d.f("parent"), Value: 1}}},
		}
		if c.FullText {
			models = append(models, mongo.IndexModel{Keys: bson.D{{Key: "text", Value: "text"}},
				Options: options.Index().SetDefaultLanguage("none")})
		}
		if _, err := coll.Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("mongo: ensure %s: %w", c.Name, err)
		}
	}
	return nil
}

// Tx implements store.Driver: a transaction on a replica set, else fn directly.
func (d *Driver) Tx(ctx context.Context, fn func(ctx context.Context) error) error {
	if !d.tx || mongo.SessionFromContext(ctx) != nil {
		return fn(ctx)
	}
	sess, err := d.client.StartSession()
	if err != nil {
		return fmt.Errorf("mongo: session: %w", err)
	}
	defer sess.EndSession(ctx)
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		return nil, fn(ctx)
	})
	return err
}

// toBSON converts JSON to a BSON document, keeping key order.
func toBSON(raw json.RawMessage) (bson.D, error) {
	var doc bson.D
	if err := bson.UnmarshalExtJSON(raw, false, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// writeJSON writes a BSON value back as JSON. Numbers are written the way
// they arrived: integers as integers, doubles as the shortest decimal that
// reads back the same, so ScreenJSON survives byte for byte (R-VER-02).
func writeJSON(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case bson.D:
		b.WriteByte('{')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(e.Key)
			b.Write(k)
			b.WriteByte(':')
			if err := writeJSON(b, e.Value); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case bson.A:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJSON(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case string:
		enc := json.NewEncoder(b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return err
		}
		b.Truncate(b.Len() - 1)
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("mongo: %v is not JSON", x)
		}
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("mongo: unexpected BSON value %T", v)
	}
	return nil
}

func fromBSON(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	if err := writeJSON(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// envelope is the stored envelope.
type envelope struct {
	Doc         string    `bson:"doc"`
	Parent      string    `bson:"parent"`
	Kind        string    `bson:"kind"`
	Type        string    `bson:"type"`
	Order       string    `bson:"order"`
	Rev         int64     `bson:"rev"`
	TextRev     int64     `bson:"text_rev"`
	CRev        int64     `bson:"crev"`
	Seq         int64     `bson:"seq"`
	Updated     time.Time `bson:"updated"`
	Layout      string    `bson:"layout"`
	VectorModel string    `bson:"vector_model,omitempty"`
	VectorMeta  string    `bson:"vector_meta,omitempty"`
}

func (d *Driver) encode(r store.Record) (bson.D, error) {
	out := bson.D{{Key: "_id", Value: r.ID}, {Key: d.env, Value: envelope{
		Doc: r.Doc, Parent: r.Parent, Kind: r.Kind, Type: r.Type, Order: r.Order,
		Rev: int64(r.Rev), TextRev: int64(r.TextRev), CRev: int64(r.CRev), Seq: int64(r.Seq),
		Updated: r.Updated, Layout: r.Layout, VectorModel: r.VectorModel, VectorMeta: string(r.VectorMeta),
	}}}
	if len(r.Node) > 0 {
		node, err := toBSON(r.Node)
		if err != nil {
			return nil, fmt.Errorf("mongo: node of %s: %w", r.ID, err)
		}
		out = append(out, bson.E{Key: "node", Value: node})
	}
	out = append(out, bson.E{Key: "text", Value: r.Text})
	if r.Vector != nil {
		v := make(bson.A, len(r.Vector))
		for i, f := range r.Vector {
			v[i] = float64(f)
		}
		out = append(out, bson.E{Key: "vector", Value: v})
	}
	return out, nil
}

func (d *Driver) decode(raw bson.Raw) (store.Record, error) {
	var doc bson.D
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return store.Record{}, err
	}
	var r store.Record
	for _, e := range doc {
		switch e.Key {
		case "_id":
			r.ID, _ = e.Value.(string)
		case d.env:
			var env envelope
			b, err := bson.Marshal(e.Value)
			if err != nil {
				return r, err
			}
			if err := bson.Unmarshal(b, &env); err != nil {
				return r, err
			}
			r.Doc, r.Parent, r.Kind, r.Type, r.Order = env.Doc, env.Parent, env.Kind, env.Type, env.Order
			r.Rev, r.TextRev, r.CRev, r.Seq = uint64(env.Rev), uint64(env.TextRev), uint64(env.CRev), uint64(env.Seq)
			r.Updated, r.Layout, r.VectorModel = env.Updated.UTC(), env.Layout, env.VectorModel
			if env.VectorMeta != "" {
				r.VectorMeta = json.RawMessage(env.VectorMeta)
			}
		case "node":
			node, err := fromBSON(e.Value)
			if err != nil {
				return r, err
			}
			r.Node = node
		case "text":
			r.Text, _ = e.Value.(string)
		case "vector":
			arr, _ := e.Value.(bson.A)
			r.Vector = make([]float32, len(arr))
			for i, x := range arr {
				switch n := x.(type) {
				case float64:
					r.Vector[i] = float32(n)
				case int32:
					r.Vector[i] = float32(n)
				case int64:
					r.Vector[i] = float32(n)
				}
			}
		}
	}
	if r.Updated.Equal(time.Unix(0, 0).UTC()) {
		r.Updated = time.Time{}
	}
	return r, nil
}

// Put implements store.Driver: bulk replaces with upsert.
func (d *Driver) Put(ctx context.Context, col string, recs []store.Record) error {
	if len(recs) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(recs))
	for _, r := range recs {
		doc, err := d.encode(r)
		if err != nil {
			return err
		}
		models = append(models, mongo.NewReplaceOneModel().SetFilter(bson.D{{Key: "_id", Value: r.ID}}).
			SetReplacement(doc).SetUpsert(true))
	}
	if _, err := d.db.Collection(col).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(true)); err != nil {
		return fmt.Errorf("mongo: put into %s: %w", col, err)
	}
	return nil
}

func (d *Driver) all(ctx context.Context, cur *mongo.Cursor) ([]store.Record, error) {
	defer cur.Close(ctx)
	var out []store.Record
	for cur.Next(ctx) {
		r, err := d.decode(cur.Current)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, cur.Err()
}

// Get implements store.Driver.
func (d *Driver) Get(ctx context.Context, col string, ids []string) ([]store.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cur, err := d.db.Collection(col).Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, fmt.Errorf("mongo: get from %s: %w", col, err)
	}
	return d.all(ctx, cur)
}

func (d *Driver) filter(f store.Filter) bson.D {
	q := bson.D{}
	if f.Doc != "" {
		q = append(q, bson.E{Key: d.f("doc"), Value: f.Doc})
	}
	if f.Parent != "" {
		q = append(q, bson.E{Key: d.f("parent"), Value: f.Parent})
	}
	if f.Kind != "" {
		q = append(q, bson.E{Key: d.f("kind"), Value: f.Kind})
	}
	if f.Type != "" {
		q = append(q, bson.E{Key: d.f("type"), Value: f.Type})
	}
	if len(f.IDs) > 0 {
		q = append(q, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: f.IDs}}})
	}
	return q
}

// Find implements store.Driver, paging by _id.
func (d *Driver) Find(ctx context.Context, col string, f store.Filter, after string, limit int) ([]store.Record, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("mongo: limit must be positive")
	}
	q := d.filter(f)
	if after != "" {
		q = append(q, bson.E{Key: "_id", Value: bson.D{{Key: "$gt", Value: after}}})
		if len(f.IDs) > 0 {
			q = bson.D{{Key: "$and", Value: bson.A{d.filter(f), bson.D{{Key: "_id", Value: bson.D{{Key: "$gt", Value: after}}}}}}}
		}
	}
	opts := options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit + 1))
	cur, err := d.db.Collection(col).Find(ctx, q, opts)
	if err != nil {
		return nil, "", fmt.Errorf("mongo: find in %s: %w", col, err)
	}
	recs, err := d.all(ctx, cur)
	if err != nil {
		return nil, "", err
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
	if _, err := d.db.Collection(col).DeleteMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}); err != nil {
		return fmt.Errorf("mongo: delete from %s: %w", col, err)
	}
	return nil
}

// DeleteWhere implements store.Driver.
func (d *Driver) DeleteWhere(ctx context.Context, col string, f store.Filter) error {
	if _, err := d.db.Collection(col).DeleteMany(ctx, d.filter(f)); err != nil {
		return fmt.Errorf("mongo: delete from %s: %w", col, err)
	}
	return nil
}

// SearchText implements store.Driver with the text index.
func (d *Driver) SearchText(ctx context.Context, col, q string, f store.Filter, k int) ([]store.Hit, error) {
	query := append(bson.D{{Key: "$text", Value: bson.D{{Key: "$search", Value: q}}}}, d.filter(f)...)
	opts := options.Find().SetProjection(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).
		SetSort(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).SetLimit(int64(k))
	// The projection above would hide the other fields; include them.
	opts.SetProjection(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}},
		{Key: "_id", Value: 1}, {Key: d.env, Value: 1}, {Key: "node", Value: 1}, {Key: "text", Value: 1}, {Key: "vector", Value: 1}})
	cur, err := d.db.Collection(col).Find(ctx, query, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo: text search in %s: %w", col, err)
	}
	defer cur.Close(ctx)
	var out []store.Hit
	for cur.Next(ctx) {
		r, err := d.decode(cur.Current)
		if err != nil {
			return nil, err
		}
		score, _ := cur.Current.Lookup("score").DoubleOK()
		out = append(out, store.Hit{Record: r, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, cur.Err()
}

// SearchVector implements store.Driver: unsupported on Mongo (a 501).
func (d *Driver) SearchVector(context.Context, string, string, []float32, store.Filter, int) ([]store.Hit, error) {
	return nil, store.ErrUnsupported
}

// Ping implements store.Driver.
func (d *Driver) Ping(ctx context.Context) error { return d.client.Ping(ctx, nil) }

// Close implements store.Driver.
func (d *Driver) Close() error { return d.client.Disconnect(context.Background()) }
