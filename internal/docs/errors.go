// Package docs is the document manager (SPEC.md 4.4, 4.5): the cache of open
// documents, one ordered write queue per document, checkouts and passes,
// validation, and the data the server derives. The API layer turns requests
// into calls on a Manager and responses back into HTTP.
package docs

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/screenjson/screenjson-db-importer/internal/schema"
)

// Error is a request the manager refuses, with the HTTP status and error code
// of SPEC.md 6.2.
type Error struct {
	Status  int
	Code    string
	Message string
	// Details are {pointer, message} defects for 422 invalid, or hints.
	Details []schema.Defect
	// Extra holds code-specific fields for the error body: holder and expires
	// for checked_out, ids for reference, supported for an unknown format.
	Extra map[string]any
	// Header holds response headers, such as Retry-After on 429.
	Header map[string]string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newErr(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error codes from SPEC.md 6.2.
const (
	CodeBadRequest     = "bad_request"
	CodeTokenRequired  = "token_required"
	CodeNotFound       = "not_found"
	CodeReference      = "reference"
	CodeLockedNode     = "locked_node"
	CodeEncrypted      = "encrypted"
	CodeDuplicateID    = "duplicate_id"
	CodeVersion        = "version_unsupported"
	CodeGone           = "gone"
	CodeRevMismatch    = "rev_mismatch"
	CodeTooLarge       = "too_large"
	CodeInvalid        = "invalid"
	CodeUseSubroute    = "use_subroute"
	CodeCheckedOut     = "checked_out"
	CodePrecondition   = "precondition_required"
	CodeBusy           = "busy"
	CodeUnsupported    = "unsupported"
	CodeStorage        = "storage"
	CodeNotReady       = "not_ready"
	CodeMethodNotAllow = "method_not_allowed"
)

func badRequest(format string, args ...any) *Error {
	return newErr(http.StatusBadRequest, CodeBadRequest, format, args...)
}

func notFound(format string, args ...any) *Error {
	return newErr(http.StatusNotFound, CodeNotFound, format, args...)
}

func invalid(msg string, defects []schema.Defect) *Error {
	e := newErr(http.StatusUnprocessableEntity, CodeInvalid, "%s", msg)
	e.Details = defects
	return e
}

func invalidf(pointer, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	return invalid(msg, []schema.Defect{{Pointer: pointer, Message: msg}})
}

// uuidGen makes UUIDv7s: a millisecond timestamp, then random bits, with a
// counter in the random part so IDs made in the same millisecond still sort in
// the order they were made.
type uuidGen struct {
	mu   sync.Mutex
	last int64
	seq  uint16
}

func (g *uuidGen) next() string {
	g.mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= g.last {
		ms = g.last
		g.seq++
	} else {
		g.last, g.seq = ms, 0
	}
	seq := g.seq
	g.mu.Unlock()

	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("docs: random UUID: %v", err))
	}
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | byte(seq>>8)&0x0f
	b[7] = byte(seq)
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var sharedUUIDs uuidGen

// NewUUID makes a UUIDv7, for records the server keeps beside documents.
func NewUUID() string { return sharedUUIDs.next() }
