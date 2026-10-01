package docs

import (
	"encoding/json"
	"time"
)

// Event ops (SPEC.md 8.3).
const (
	OpUpdated          = "node.updated"
	OpInserted         = "node.inserted"
	OpRemoved          = "node.removed"
	OpMoved            = "node.moved"
	OpReplaced         = "document.replaced"
	OpCreated          = "document.created"
	OpDeleted          = "document.deleted"
	OpCheckoutChanged  = "checkout.changed"
	OpTextChangedHook  = "text.changed"
	eventTimeFormatISO = time.RFC3339Nano
)

// Event is one change to a document, as published on doc:{id} (SPEC.md 8.3).
//
// Node events are about nodes of the tree proper — the root, scenes, elements,
// characters and analysis. A change to an embedded object such as a note or an
// author is reported as an update of the tree node that holds it. An event's
// node is the node's ScreenJSON without its splittable children, except that a
// scene insert carries its body; analysis updates leave out embeddings, which
// are large and never part of a default GET.
type Event struct {
	Seq     uint64          `json:"seq"`
	Doc     string          `json:"doc"`
	Op      string          `json:"op"`
	Kind    string          `json:"kind,omitempty"`
	Type    string          `json:"type,omitempty"`
	ID      string          `json:"id,omitempty"`
	Path    string          `json:"path,omitempty"`
	OldPath string          `json:"old_path,omitempty"`
	Parent  string          `json:"parent,omitempty"`
	After   *string         `json:"after,omitempty"`
	Rev     uint64          `json:"rev,omitempty"`
	TextRev uint64          `json:"text_rev,omitempty"`
	Node    json.RawMessage `json:"node,omitempty"`
	Actor   string          `json:"actor,omitempty"`
	TS      time.Time       `json:"ts"`
	// Checkout events.
	NodeID  string     `json:"node_id,omitempty"`
	Holder  string     `json:"holder,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
	// Order is the node's order key, which lets a replay place it exactly.
	Order string `json:"order,omitempty"`
}

// Publisher receives events once their write is committed, in seq order for
// each document (R-EVT-05). The events package implements it.
type Publisher interface {
	// Publish sends a document's events.
	Publish(doc string, events []Event)
	// Checkout sends a checkout.changed event, which has no seq.
	Checkout(doc string, ev Event)
	// Subscribed reports whether anyone is watching a document, which keeps
	// it in the cache (R-ARCH-09).
	Subscribed(doc string) bool
}

// HookEvent is a change a webhook may be told about (SPEC.md 9).
type HookEvent struct {
	Event   string
	Doc     string
	ID      string
	Kind    string
	Type    string
	Path    string
	TextRev uint64
	Langs   []string
	// Text is nil for encrypted nodes, which are marked Encrypted instead
	// (R-ENC-03, R-HOOK-02).
	Text      map[string]any
	Encrypted bool
	EmbedURL  string
}

// Hooks queues webhook deliveries. It must never block (R-HOOK-03).
type Hooks interface {
	Enqueue(ev HookEvent)
}

// nopPublisher and nopHooks stand in when nothing is configured.
type nopPublisher struct{}

func (nopPublisher) Publish(string, []Event) {}
func (nopPublisher) Checkout(string, Event)  {}
func (nopPublisher) Subscribed(string) bool  { return false }

type nopHooks struct{}

func (nopHooks) Enqueue(HookEvent) {}
