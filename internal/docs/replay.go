package docs

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrRefetch means an event can't be applied to a local copy — a whole
// document was replaced — and the client should fetch the document again.
var ErrRefetch = errors.New("docs: refetch the document")

// Apply plays one event onto a document held as decoded JSON, as a client
// keeping a live copy does (SPEC.md 8.3). The web UI does the same in
// TypeScript. Replaying a document's events in seq order onto the document as
// it was gives the document as it is, analysis aside (R-VER-04).
func Apply(doc map[string]any, ev Event) error {
	switch ev.Op {
	case OpCreated, OpDeleted, OpCheckoutChanged:
		return nil
	case OpReplaced:
		return ErrRefetch
	}
	var node map[string]any
	if len(ev.Node) > 0 {
		if err := json.Unmarshal(ev.Node, &node); err != nil {
			return fmt.Errorf("event %d: node: %w", ev.Seq, err)
		}
	}
	switch ev.Op {
	case OpUpdated:
		return applyUpdate(doc, ev, node)
	case OpInserted:
		return applyInsert(doc, ev.Kind, ev.Parent, ev.After, node)
	case OpRemoved:
		if ev.Kind == "analysis" {
			delete(doc, "analysis")
			return nil
		}
		if _, ok := detach(doc, ev.ID); !ok {
			return fmt.Errorf("event %d: no node %s to remove", ev.Seq, ev.ID)
		}
		return nil
	case OpMoved:
		n, ok := detach(doc, ev.ID)
		if !ok {
			return fmt.Errorf("event %d: no node %s to move", ev.Seq, ev.ID)
		}
		if ev.Kind == "element" {
			n["scene"] = ev.Parent
		}
		return applyInsert(doc, ev.Kind, ev.Parent, ev.After, n)
	}
	return fmt.Errorf("event %d: unknown op %q", ev.Seq, ev.Op)
}

func applyUpdate(doc map[string]any, ev Event, node map[string]any) error {
	switch ev.Kind {
	case "document":
		// The root's event leaves out its splittable children; keep ours.
		wrapper, _ := doc["document"].(map[string]any)
		scenes := wrapper["scenes"]
		chars, hasChars := doc["characters"]
		analysis, hasAnalysis := doc["analysis"]
		for k := range doc {
			delete(doc, k)
		}
		for k, v := range node {
			doc[k] = v
		}
		if w, ok := doc["document"].(map[string]any); ok {
			w["scenes"] = scenes
		}
		if hasChars {
			doc["characters"] = chars
		}
		if hasAnalysis {
			doc["analysis"] = analysis
		}
		return nil
	case "analysis":
		old, _ := doc["analysis"].(map[string]any)
		if emb, ok := old["embeddings"]; ok {
			node["embeddings"] = emb
		}
		doc["analysis"] = node
		return nil
	case "scene":
		s := findScene(doc, ev.ID)
		if s == nil {
			return fmt.Errorf("event %d: no scene %s", ev.Seq, ev.ID)
		}
		body := s["body"]
		for k := range s {
			delete(s, k)
		}
		for k, v := range node {
			s[k] = v
		}
		s["body"] = body
		return nil
	case "element", "character":
		list, i := locateItem(doc, ev.ID)
		if list == nil {
			return fmt.Errorf("event %d: no %s %s", ev.Seq, ev.Kind, ev.ID)
		}
		(*list)[i] = node
		return nil
	}
	return fmt.Errorf("event %d: can't update a %s", ev.Seq, ev.Kind)
}

// applyInsert puts a node after a sibling (first when after is nil).
func applyInsert(doc map[string]any, kind, parent string, after *string, node map[string]any) error {
	var container map[string]any
	var key string
	switch kind {
	case "analysis":
		doc["analysis"] = node
		return nil
	case "scene":
		container, _ = doc["document"].(map[string]any)
		key = "scenes"
	case "element":
		container, key = findScene(doc, parent), "body"
		if container == nil {
			return fmt.Errorf("no scene %s to insert into", parent)
		}
	case "character":
		container, key = doc, "characters"
	default:
		return fmt.Errorf("can't insert a %s", kind)
	}
	list, _ := container[key].([]any)
	at := 0
	if after != nil {
		at = -1
		for i, x := range list {
			if m, ok := x.(map[string]any); ok && m["id"] == *after {
				at = i + 1
			}
		}
		if at < 0 {
			return fmt.Errorf("no sibling %s to insert after", *after)
		}
	}
	out := make([]any, 0, len(list)+1)
	out = append(out, list[:at]...)
	out = append(out, node)
	out = append(out, list[at:]...)
	container[key] = out
	return nil
}

func findScene(doc map[string]any, id string) map[string]any {
	wrapper, _ := doc["document"].(map[string]any)
	scenes, _ := wrapper["scenes"].([]any)
	for _, s := range scenes {
		if m, ok := s.(map[string]any); ok && m["id"] == id {
			return m
		}
	}
	return nil
}

// locateItem finds an element or character by ID.
func locateItem(doc map[string]any, id string) (*[]any, int) {
	chars, _ := doc["characters"].([]any)
	for i, c := range chars {
		if m, ok := c.(map[string]any); ok && m["id"] == id {
			return &chars, i
		}
	}
	wrapper, _ := doc["document"].(map[string]any)
	scenes, _ := wrapper["scenes"].([]any)
	for _, s := range scenes {
		sm, _ := s.(map[string]any)
		body, _ := sm["body"].([]any)
		for i, e := range body {
			if m, ok := e.(map[string]any); ok && m["id"] == id {
				return &body, i
			}
		}
	}
	return nil, 0
}

// detach removes a scene, element or character from wherever it is.
func detach(doc map[string]any, id string) (map[string]any, bool) {
	cut := func(m map[string]any, key string) (map[string]any, bool) {
		list, _ := m[key].([]any)
		for i, x := range list {
			if n, ok := x.(map[string]any); ok && n["id"] == id {
				m[key] = append(list[:i:i], list[i+1:]...)
				return n, true
			}
		}
		return nil, false
	}
	if n, ok := cut(doc, "characters"); ok {
		return n, true
	}
	wrapper, _ := doc["document"].(map[string]any)
	if n, ok := cut(wrapper, "scenes"); ok {
		return n, true
	}
	scenes, _ := wrapper["scenes"].([]any)
	for _, s := range scenes {
		if n, ok := cut(s.(map[string]any), "body"); ok {
			return n, true
		}
	}
	return nil, false
}
