// Package state is the embedded bbolt store (SPEC.md 4.7): tokens, pass
// progress, the layout fingerprint, relayout progress and the document index.
// bbolt locks its file, so offline commands can't run while the server holds
// it (R-STATE-01, R-CLI-07).
package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// ErrLocked means another process — normally the running server — holds the
// state file.
var ErrLocked = errors.New("state: the state store is locked by another process; stop the server before running offline commands")

// ErrNotFound means the key is not in the store.
var ErrNotFound = errors.New("state: not found")

// Bucket names.
var (
	bucketTokens  = []byte("tokens")
	bucketPasses  = []byte("passes")
	bucketDone    = []byte("pass_done")
	bucketDocs    = []byte("docs")
	bucketMeta    = []byte("meta")
	bucketRelay   = []byte("relayout")
	allBuckets    = [][]byte{bucketTokens, bucketPasses, bucketDone, bucketDocs, bucketMeta, bucketRelay}
	keyLayout     = []byte("layout_fingerprint")
	keyLayoutFull = []byte("layout_normalised")
	keyInstance   = []byte("instance")
)

// Store is an open state file.
type Store struct {
	db *bolt.DB
	// now is the clock, replaceable in tests.
	now func() time.Time

	// The document index is held in memory and written behind: every API
	// write updates it (R-DER-04), and a disk write per API write would make
	// the index the bottleneck. Changes reach the file within FlushInterval
	// and on Close.
	idxMu    sync.RWMutex
	idx      map[string]DocEntry
	idxDirty map[string]bool // true = put, false = delete
	// idxLazy holds entries not built yet: a write records how to build its
	// document's entry, and a read of the entry builds it once, however many
	// writes came in between.
	idxLazy  map[string]func() DocEntry
	stop     chan struct{}
	stopped  chan struct{}
	closing  sync.Once
	closeErr error
}

// FlushInterval is how often index changes are written to the file.
const FlushInterval = time.Second

// Open opens or creates ${dir}/state.db. It waits a second for the lock and
// then gives up with ErrLocked.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("state: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "state.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("%w (%s)", ErrLocked, path)
	}
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state: create buckets: %w", err)
	}
	s := &Store{
		db: db, now: func() time.Time { return time.Now().UTC() },
		idx: map[string]DocEntry{}, idxDirty: map[string]bool{}, idxLazy: map[string]func() DocEntry{},
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
	err = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDocs).ForEach(func(k, v []byte) error {
			var e DocEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("index entry %s: %w", k, err)
			}
			s.idx[string(k)] = e
			return nil
		})
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state: load document index: %w", err)
	}
	go s.flushLoop()
	return s, nil
}

// Close writes pending index changes and releases the file and its lock.
// Closing twice is harmless.
func (s *Store) Close() error {
	s.closing.Do(func() {
		close(s.stop)
		<-s.stopped
		ferr := s.Flush()
		if err := s.db.Close(); err != nil {
			s.closeErr = err
			return
		}
		s.closeErr = ferr
	})
	return s.closeErr
}

func (s *Store) flushLoop() {
	defer close(s.stopped)
	t := time.NewTicker(FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			_ = s.Flush()
		}
	}
}

// Flush writes pending document index changes to the file now.
func (s *Store) Flush() error {
	s.idxMu.Lock()
	s.resolveLocked()
	if len(s.idxDirty) == 0 {
		s.idxMu.Unlock()
		return nil
	}
	dirty := s.idxDirty
	s.idxDirty = map[string]bool{}
	entries := make(map[string]DocEntry, len(dirty))
	for id, isPut := range dirty {
		if isPut {
			entries[id] = s.idx[id]
		}
	}
	s.idxMu.Unlock()

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDocs)
		for id, isPut := range dirty {
			if !isPut {
				if err := b.Delete([]byte(id)); err != nil {
					return err
				}
				continue
			}
			if err := put(tx, bucketDocs, []byte(id), entries[id]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Put the changes back so the next flush tries again.
		s.idxMu.Lock()
		for id, isPut := range dirty {
			if _, newer := s.idxDirty[id]; !newer {
				s.idxDirty[id] = isPut
			}
		}
		s.idxMu.Unlock()
		return fmt.Errorf("state: flush document index: %w", err)
	}
	return nil
}

// Path is the file's location.
func (s *Store) Path() string { return s.db.Path() }

func put(tx *bolt.Tx, bucket, key []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put(key, b)
}

func get(tx *bolt.Tx, bucket, key []byte, v any) error {
	b := tx.Bucket(bucket).Get(key)
	if b == nil {
		return ErrNotFound
	}
	return json.Unmarshal(b, v)
}

// ---- Layout fingerprint (SPEC.md 5.3) ----

// Layout returns the stored fingerprint and normalised layout, or ErrNotFound
// on a fresh store.
func (s *Store) Layout() (fingerprint string, normalised []byte, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		fp := b.Get(keyLayout)
		if fp == nil {
			return ErrNotFound
		}
		fingerprint = string(fp)
		normalised = slices.Clone(b.Get(keyLayoutFull))
		return nil
	})
	return fingerprint, normalised, err
}

// Instance returns this data directory's server instance ID, making one with
// fresh the first time. The lease is held under it, so a server restarted on
// the same data directory recognises its own lease (R-ARCH-11).
func (s *Store) Instance(fresh func() string) (string, error) {
	var id string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if v := b.Get(keyInstance); v != nil {
			id = string(v)
			return nil
		}
		id = fresh()
		return b.Put(keyInstance, []byte(id))
	})
	return id, err
}

// SetLayout stores the fingerprint and the full layout next to it, which
// relayout needs to read documents written the old way (R-STORE-07).
func (s *Store) SetLayout(fingerprint string, normalised []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if err := b.Put(keyLayout, []byte(fingerprint)); err != nil {
			return err
		}
		return b.Put(keyLayoutFull, normalised)
	})
}

// ---- Relayout progress (R-STORE-07) ----

// RelayoutDone reports whether relayout to the target fingerprint has already
// rewritten a document.
func (s *Store) RelayoutDone(target, doc string) (bool, error) {
	var done bool
	err := s.db.View(func(tx *bolt.Tx) error {
		done = tx.Bucket(bucketRelay).Get([]byte(target+"/"+doc)) != nil
		return nil
	})
	return done, err
}

// MarkRelayout records that relayout rewrote a document.
func (s *Store) MarkRelayout(target, doc string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketRelay).Put([]byte(target+"/"+doc), []byte{1})
	})
}

// ClearRelayout forgets all relayout progress, once a relayout has finished.
func (s *Store) ClearRelayout() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(bucketRelay); err != nil {
			return err
		}
		_, err := tx.CreateBucket(bucketRelay)
		return err
	})
}

// ---- Tokens (SPEC.md 6.3) ----

// Token is a stored token. Only the SHA-256 of the secret is kept (R-TOK-02).
type Token struct {
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
}

// TokenPrefix starts every token secret.
const TokenPrefix = "sjt_"

// NewSecret makes a token secret: 32 random bytes in base62, prefixed sjt_.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("state: random token: %w", err)
	}
	return TokenPrefix + new(big.Int).SetBytes(b).Text(62), nil
}

// HashSecret is what is stored and looked up.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken stores a new token and returns it with its secret, which is
// never shown again. Names may repeat.
func (s *Store) CreateToken(name string) (Token, string, error) {
	if name == "" {
		return Token{}, "", errors.New("state: a token needs a name")
	}
	secret, err := NewSecret()
	if err != nil {
		return Token{}, "", err
	}
	t := Token{Name: name, Hash: HashSecret(secret), Created: s.now()}
	err = s.db.Update(func(tx *bolt.Tx) error {
		return put(tx, bucketTokens, []byte(t.Hash), t)
	})
	if err != nil {
		return Token{}, "", fmt.Errorf("state: store token: %w", err)
	}
	return t, secret, nil
}

// LookupToken finds a token by its secret and notes when it was last used.
// The last-used time is written at most once a minute per token, so busy
// callers don't turn every request into a disk write.
func (s *Store) LookupToken(secret string) (Token, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Token{}, ErrNotFound
	}
	hash := []byte(HashSecret(secret))
	var t Token
	if err := s.db.View(func(tx *bolt.Tx) error { return get(tx, bucketTokens, hash, &t) }); err != nil {
		return Token{}, err
	}
	now := s.now()
	if now.Sub(t.LastUsed) >= time.Minute {
		t.LastUsed = now
		if err := s.db.Update(func(tx *bolt.Tx) error { return put(tx, bucketTokens, hash, t) }); err != nil {
			return Token{}, fmt.Errorf("state: update token: %w", err)
		}
	}
	return t, nil
}

// Tokens lists every token, oldest first, without secrets.
func (s *Store) Tokens() ([]Token, error) {
	var out []Token
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketTokens).ForEach(func(_, v []byte) error {
			var t Token
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			out = append(out, t)
			return nil
		})
	})
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].Hash < out[j].Hash
	})
	return out, err
}

// DeleteTokens removes every token with the name and reports how many went.
func (s *Store) DeleteTokens(name string) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTokens)
		var doomed [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var t Token
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if t.Name == name {
				doomed = append(doomed, slices.Clone(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range doomed {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		n = len(doomed)
		return nil
	})
	return n, err
}

// ---- Passes (SPEC.md 6.9) ----

// Pass is one sweep of work's progress.
type Pass struct {
	Label   string    `json:"label"`
	Done    int       `json:"done"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Cursor is the last document a library-wide bulk checkout finished
	// scanning, so later calls start after it (R-CO-11).
	Cursor string `json:"cursor,omitempty"`
}

// passKey is how a finished node is keyed: label, then node.
func passKey(label, node string) []byte { return []byte(label + "\x00" + node) }

// GetPass returns a pass, or ErrNotFound.
func (s *Store) GetPass(label string) (Pass, error) {
	var p Pass
	err := s.db.View(func(tx *bolt.Tx) error { return get(tx, bucketPasses, []byte(label), &p) })
	return p, err
}

// Passes lists every pass by label.
func (s *Store) Passes() ([]Pass, error) {
	var out []Pass
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPasses).ForEach(func(_, v []byte) error {
			var p Pass
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	})
	return out, err
}

// touchPass loads or creates a pass inside a transaction.
func (s *Store) touchPass(tx *bolt.Tx, label string) (Pass, error) {
	var p Pass
	err := get(tx, bucketPasses, []byte(label), &p)
	if errors.Is(err, ErrNotFound) {
		now := s.now()
		return Pass{Label: label, Created: now, Updated: now}, nil
	}
	return p, err
}

// EnsurePass creates a pass if it doesn't exist yet. An existing pass is only
// read: every bulk checkout calls this, and a write would cost a disk sync.
func (s *Store) EnsurePass(label string) (Pass, error) {
	if p, err := s.GetPass(label); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return p, err
	}
	var p Pass
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		if p, err = s.touchPass(tx, label); err != nil {
			return err
		}
		return put(tx, bucketPasses, []byte(label), p)
	})
	return p, err
}

// MarkDone records nodes as finished for a pass (R-CO-09). Nodes already done
// are not counted twice.
func (s *Store) MarkDone(label string, nodes ...string) error {
	// Batch merges concurrent callers into one transaction: many workers
	// finishing nodes at once cost one disk write, not one each.
	return s.db.Batch(func(tx *bolt.Tx) error {
		p, err := s.touchPass(tx, label)
		if err != nil {
			return err
		}
		done := tx.Bucket(bucketDone)
		for _, n := range nodes {
			k := passKey(label, n)
			if done.Get(k) != nil {
				continue
			}
			if err := done.Put(k, []byte{1}); err != nil {
				return err
			}
			p.Done++
		}
		p.Updated = s.now()
		return put(tx, bucketPasses, []byte(label), p)
	})
}

// IsDone reports whether a pass has finished a node.
func (s *Store) IsDone(label, node string) (bool, error) {
	var done bool
	err := s.db.View(func(tx *bolt.Tx) error {
		done = tx.Bucket(bucketDone).Get(passKey(label, node)) != nil
		return nil
	})
	return done, err
}

// DoneSet returns every node a pass has finished, for filtering a bulk
// checkout without a read per node.
func (s *Store) DoneSet(label string) (map[string]bool, error) {
	out := map[string]bool{}
	prefix := []byte(label + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketDone).Cursor()
		for k, _ := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, _ = c.Next() {
			out[string(k[len(prefix):])] = true
		}
		return nil
	})
	return out, err
}

// SetPassCursor records how far a library-wide scan got.
func (s *Store) SetPassCursor(label, cursor string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, err := s.touchPass(tx, label)
		if err != nil {
			return err
		}
		p.Cursor, p.Updated = cursor, s.now()
		return put(tx, bucketPasses, []byte(label), p)
	})
}

// DeletePass forgets a pass and everything it finished (R-CO-13).
func (s *Store) DeletePass(label string) error {
	prefix := []byte(label + "\x00")
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketPasses).Get([]byte(label)) == nil {
			return ErrNotFound
		}
		if err := tx.Bucket(bucketPasses).Delete([]byte(label)); err != nil {
			return err
		}
		c := tx.Bucket(bucketDone).Cursor()
		for k, _ := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, _ = c.Seek(prefix) {
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- Document index (SPEC.md 4.7, 6.13) ----

// DocEntry is one document in the index.
type DocEntry struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Logline  string    `json:"logline,omitempty"`
	Authors  []string  `json:"authors"`
	Genre    []string  `json:"genre"`
	Themes   []string  `json:"themes"`
	Lang     string    `json:"lang"`
	Langs    []string  `json:"langs"`
	Scenes   int       `json:"scenes"`
	Elements int       `json:"elements"`
	Seq      uint64    `json:"seq"`
	Updated  time.Time `json:"updated"`
	Bytes    int64     `json:"bytes"`
	Commit   string    `json:"commit,omitempty"`
}

// PutDoc adds or replaces a document's entry. A Git commit already recorded
// is kept when e has none.
func (s *Store) PutDoc(e DocEntry) error {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	delete(s.idxLazy, e.ID)
	s.putLocked(e)
	return nil
}

// PutDocLazy records that a document's entry changed, with how to build it;
// it is built when next read or flushed.
func (s *Store) PutDocLazy(id string, build func() DocEntry) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	s.idxLazy[id] = build
	if _, ok := s.idx[id]; !ok {
		s.idx[id] = DocEntry{ID: id}
	}
	s.idxDirty[id] = true
}

func (s *Store) putLocked(e DocEntry) {
	if old, ok := s.idx[e.ID]; ok && e.Commit == "" {
		e.Commit = old.Commit
	}
	s.idx[e.ID] = e
	s.idxDirty[e.ID] = true
}

// resolveLocked builds the entries waiting in idxLazy. Call with idxMu held.
func (s *Store) resolveLocked() {
	for id, build := range s.idxLazy {
		s.putLocked(build())
		delete(s.idxLazy, id)
	}
}

// GetDoc returns one entry, or ErrNotFound.
func (s *Store) GetDoc(id string) (DocEntry, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if build, ok := s.idxLazy[id]; ok {
		s.putLocked(build())
		delete(s.idxLazy, id)
	}
	e, ok := s.idx[id]
	if !ok {
		return DocEntry{}, ErrNotFound
	}
	return e, nil
}

// DeleteDoc removes an entry.
func (s *Store) DeleteDoc(id string) error {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	delete(s.idxLazy, id)
	delete(s.idx, id)
	s.idxDirty[id] = false
	return nil
}

// SetCommit records the Git commit of a document's last export.
func (s *Store) SetCommit(id, commit string) error {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	s.resolveLocked()
	e, ok := s.idx[id]
	if !ok {
		return ErrNotFound
	}
	e.Commit = commit
	s.idx[id] = e
	s.idxDirty[id] = true
	return nil
}

// DocIDs lists every indexed document in ID order: the fixed order
// library-wide passes walk in (R-CO-11).
func (s *Store) DocIDs() ([]string, error) {
	s.idxMu.RLock()
	out := make([]string, 0, len(s.idx))
	for id := range s.idx {
		out = append(out, id)
	}
	s.idxMu.RUnlock()
	sort.Strings(out)
	return out, nil
}

// ClearDocs empties the index, for reindex (R-STATE-02).
func (s *Store) ClearDocs() error {
	s.idxMu.Lock()
	for id := range s.idx {
		s.idxDirty[id] = false
	}
	s.idx = map[string]DocEntry{}
	s.idxLazy = map[string]func() DocEntry{}
	s.idxMu.Unlock()
	return s.Flush()
}

// Query selects and sorts index entries for GET /documents.
type Query struct {
	// Q matches title or logline, ignoring case (R-SRCH-04).
	Q string
	// Genre and Theme must be among the document's.
	Genre, Theme string
	// Lang must be the document's language or one it contains.
	Lang string
	// Sort is the column: title (the default), authors, genre, lang,
	// scenes, elements, bytes or updated. A leading "-" sorts descending
	// and "+" ascending; otherwise Order decides.
	Sort string
	// Order is asc or desc. Left empty, text columns sort ascending and
	// numbers and updated descending (newest, biggest first).
	Order string
	// Limit is the page size: default 100, at most 1,000 (R-API-06).
	Limit int
	// Offset skips that many entries, for numbered pages. A Cursor wins.
	Offset int
	// Cursor continues a previous page.
	Cursor string
}

// Page is one page of results.
type Page struct {
	Items []DocEntry
	// Next continues the listing, or is "" on the last page.
	Next string
	// Total is how many entries match the query, across all pages.
	Total int
	// Offset is where this page starts in the whole result.
	Offset int
}

// Facets counts the genres, themes and languages across every indexed
// document, for the library's filter choices.
func (s *Store) Facets() map[string]map[string]int {
	out := map[string]map[string]int{"genre": {}, "theme": {}, "lang": {}}
	s.idxMu.Lock()
	s.resolveLocked()
	for _, e := range s.idx {
		for _, g := range e.Genre {
			out["genre"][g]++
		}
		for _, t := range e.Themes {
			out["theme"][t]++
		}
		langs := map[string]bool{e.Lang: true}
		for _, l := range e.Langs {
			langs[l] = true
		}
		for l := range langs {
			if l != "" {
				out["lang"][l]++
			}
		}
	}
	s.idxMu.Unlock()
	return out
}

// sortKeys are the sortable columns, each a key that orders ascending.
var sortKeys = map[string]func(DocEntry) string{
	"title":    func(e DocEntry) string { return strings.ToLower(e.Title) },
	"authors":  func(e DocEntry) string { return strings.ToLower(strings.Join(e.Authors, ", ")) },
	"genre":    func(e DocEntry) string { return strings.ToLower(strings.Join(e.Genre, ", ")) },
	"lang":     func(e DocEntry) string { return strings.ToLower(e.Lang) },
	"scenes":   func(e DocEntry) string { return fmt.Sprintf("%020d", e.Scenes) },
	"elements": func(e DocEntry) string { return fmt.Sprintf("%020d", e.Elements) },
	"bytes":    func(e DocEntry) string { return fmt.Sprintf("%020d", e.Bytes) },
	"updated":  func(e DocEntry) string { return fmt.Sprintf("%020d", e.Updated.UnixNano()) },
}

// descByDefault are the columns that sort descending unless told otherwise.
var descByDefault = map[string]bool{"scenes": true, "elements": true, "bytes": true, "updated": true}

// SortColumns lists the columns Docs can sort on.
func SortColumns() []string {
	out := make([]string, 0, len(sortKeys))
	for k := range sortKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// descending turns an ascending key into one that sorts the other way:
// each byte is complemented, and a closing 0xFF puts a longer key before
// its own prefix.
func descending(k string) string {
	b := make([]byte, len(k)+1)
	for i := 0; i < len(k); i++ {
		b[i] = 0xFF - k[i]
	}
	b[len(k)] = 0xFF
	return string(b)
}

// Docs answers a query. The cursor holds the position in the sorted result as
// the last entry's sort key and ID, so pages stay consistent while documents
// are added.
func (s *Store) Docs(q Query) (Page, error) {
	switch {
	case q.Limit <= 0:
		q.Limit = 100
	case q.Limit > 1000:
		q.Limit = 1000
	}
	col, order := q.Sort, q.Order
	switch {
	case strings.HasPrefix(col, "-"):
		col, order = col[1:], "desc"
	case strings.HasPrefix(col, "+"):
		col, order = col[1:], "asc"
	}
	if col == "" {
		col = "title"
	}
	colKey, ok := sortKeys[col]
	if !ok {
		return Page{}, fmt.Errorf("state: sort must be one of %s, not %q", strings.Join(SortColumns(), ", "), q.Sort)
	}
	if order == "" {
		order = "asc"
		if descByDefault[col] {
			order = "desc"
		}
	}
	if order != "asc" && order != "desc" {
		return Page{}, fmt.Errorf("state: order must be asc or desc, not %q", q.Order)
	}
	if q.Offset < 0 {
		return Page{}, fmt.Errorf("state: offset must not be negative")
	}

	var all []DocEntry
	needle := strings.ToLower(q.Q)
	s.idxMu.Lock()
	s.resolveLocked()
	for _, e := range s.idx {
		if needle != "" && !strings.Contains(strings.ToLower(e.Title), needle) &&
			!strings.Contains(strings.ToLower(e.Logline), needle) {
			continue
		}
		if q.Genre != "" && !slices.Contains(e.Genre, q.Genre) {
			continue
		}
		if q.Theme != "" && !slices.Contains(e.Themes, q.Theme) {
			continue
		}
		if q.Lang != "" && e.Lang != q.Lang && !slices.Contains(e.Langs, q.Lang) {
			continue
		}
		all = append(all, e)
	}
	s.idxMu.Unlock()

	key := colKey
	if order == "desc" {
		key = func(e DocEntry) string { return descending(colKey(e)) }
	}
	keys := make(map[string]string, len(all))
	for _, e := range all {
		keys[e.ID] = key(e)
	}
	sort.Slice(all, func(i, j int) bool {
		ki, kj := keys[all[i].ID], keys[all[j].ID]
		if ki != kj {
			return ki < kj
		}
		return all[i].ID < all[j].ID
	})

	start := min(q.Offset, len(all))
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return Page{}, fmt.Errorf("state: bad cursor")
		}
		hk, cid, ok := strings.Cut(string(raw), ":")
		ckb, herr := hex.DecodeString(hk)
		if !ok || herr != nil {
			return Page{}, fmt.Errorf("state: bad cursor")
		}
		ck := string(ckb)
		start = sort.Search(len(all), func(i int) bool {
			k := keys[all[i].ID]
			return k > ck || (k == ck && all[i].ID > cid)
		})
	}
	end := min(start+q.Limit, len(all))
	page := Page{Items: all[start:end], Total: len(all), Offset: start}
	if end < len(all) {
		last := all[end-1]
		page.Next = base64.RawURLEncoding.EncodeToString([]byte(hex.EncodeToString([]byte(keys[last.ID])) + ":" + last.ID))
	}
	return page, nil
}
