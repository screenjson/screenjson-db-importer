package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
)

// redirect is a 308 to a node's current path (R-API-07, R-PATH-07). It
// travels as an error so any operation can stop with it.
type redirect struct{ Location string }

func (r *redirect) Error() string { return "moved to " + r.Location }

// Redirect reports whether err is a redirect, and where to.
func Redirect(err error) (string, bool) {
	var r *redirect
	if errors.As(err, &r) {
		return r.Location, true
	}
	return "", false
}

// fromPaths converts a paths.Error into a manager error.
func fromPaths(err error) error {
	var pe *paths.Error
	if errors.As(err, &pe) {
		e := newErr(pe.Status, pe.Code, "%s", pe.Message)
		return e
	}
	return err
}

// resolve finds a target's anchor node in a tree.
func resolve(t *model.Tree, tg *paths.Target) (*model.Node, error) {
	res, err := paths.Resolve(t, tg)
	if err != nil {
		return nil, fromPaths(err)
	}
	if res.Location != "" {
		return nil, &redirect{Location: res.Location}
	}
	return res.Node, nil
}

// resolveNode loads a document and finds a target's anchor node in it.
func (m *Manager) resolveNode(ctx context.Context, tg *paths.Target) (*model.Tree, *model.Node, error) {
	t, err := m.Tree(ctx, tg.Doc())
	if err != nil {
		return nil, nil, err
	}
	n, err := resolve(t, tg)
	if err != nil {
		return nil, nil, err
	}
	return t, n, nil
}

// embeddedArray is where each kind of embedded object lives in its owner's
// Fields.
var embeddedArray = map[model.Kind][]string{
	model.KindAuthor:      {"authors"},
	model.KindContributor: {"contributors"},
	model.KindSource:      {"sources"},
	model.KindRevision:    {"revisions"},
	model.KindBookmark:    {"document", "bookmarks"},
	model.KindNote:        {"notes"},
	model.KindPassage:     {"passages"},
	model.KindSummary:     {"summaries"},
}

// loc is where a node route's data lives.
type loc struct {
	route  *paths.Route
	anchor *model.Node
	// owner is the tree node whose Fields hold the data. When path is empty
	// and key is "", the data is owner itself.
	owner *model.Node
	// path leads from owner.Fields to the data: to an object for a field route,
	// or to the array holding an item.
	path []string
	// key picks an item out of the array at path: an ID, a slug, or a
	// registration key.
	key string
	// revNode gives the ETag and text revision: the anchor for a UUID node,
	// else the owner.
	revNode *model.Node
}

// isTreeNode reports whether the location is a node of the tree itself.
func (l *loc) isTreeNode() bool { return len(l.path) == 0 && l.key == "" }

// splitPointer resolves a route pointer against its anchor. A pointer into
// analysis from the root moves to the analysis node, which is a node of the
// tree rather than part of the root's Fields.
func splitPointer(anchor *model.Node, pointer string) (*model.Node, []string, error) {
	var toks []string
	if pointer != "" {
		toks = strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	}
	if anchor.Kind == model.KindDocument && len(toks) > 0 && toks[0] == "analysis" {
		a := anchor.Analysis()
		if a == nil {
			return nil, nil, notFound("document %s has no analysis", anchor.ID)
		}
		return a, toks[1:], nil
	}
	return anchor, toks, nil
}

// locate finds a node route's data in a tree.
func locate(t *model.Tree, tg *paths.Target) (*loc, error) {
	anchor, err := resolve(t, tg)
	if err != nil {
		return nil, err
	}
	r := tg.Route
	l := &loc{route: r, anchor: anchor, owner: anchor, revNode: anchor}

	if anchor.Kind != model.KindDocument && isEmbedded(anchor) {
		// An author, note, bookmark and so on: an item in its owner's array.
		l.owner, l.revNode = anchor.Parent, anchor
		l.path, l.key = embeddedArray[anchor.Kind], anchor.ID
		return l, nil
	}
	owner, toks, err := splitPointer(anchor, r.Pointer)
	if err != nil {
		return nil, err
	}
	l.owner, l.revNode = owner, owner
	l.path = toks
	switch {
	case tg.Param(paths.ParamSlug) != "":
		l.key = tg.Param(paths.ParamSlug)
	case tg.Param(paths.ParamRegistration) != "":
		l.key = tg.Param(paths.ParamRegistration)
	}
	return l, nil
}

// isEmbedded reports whether an indexed node is an embedded object rather than
// a node of the tree.
func isEmbedded(n *model.Node) bool {
	_, ok := embeddedArray[n.Kind]
	return ok
}

// itemKey is how an item in an array is matched: registrations by
// "{authority}~{id}", everything else by id.
func itemKey(item map[string]any) string {
	id, _ := item["id"].(string)
	if auth, ok := item["authority"].(string); ok {
		return paths.RegistrationKey(auth, id)
	}
	return id
}

// walkPath follows a path of field names from a map, creating nothing.
func walkPath(root map[string]any, path []string) (any, bool) {
	var cur any = root
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// value returns the JSON at a location.
func (l *loc) value() (any, error) {
	if l.isTreeNode() {
		return l.owner.JSON()
	}
	v, ok := walkPath(l.owner.Fields, l.path)
	if !ok {
		return nil, notFound("%s has no %s", l.owner, strings.Join(l.path, "."))
	}
	if l.key == "" {
		return v, nil
	}
	list, _ := v.([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && itemKey(m) == l.key {
			return m, nil
		}
	}
	return nil, notFound("no %s %q", l.route.Kind, l.key)
}

// index finds an item's position in its array.
func (l *loc) index() (int, []any, error) {
	v, _ := walkPath(l.owner.Fields, l.path)
	list, _ := v.([]any)
	for i, item := range list {
		if m, ok := item.(map[string]any); ok && itemKey(m) == l.key {
			return i, list, nil
		}
	}
	return -1, list, notFound("no %s %q", l.route.Kind, l.key)
}

// setPath writes a value at a path of field names, creating objects on the way.
func setPath(root map[string]any, path []string, v any) {
	cur := root
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

// deletePath removes the value at a path, reporting whether it was there.
func deletePath(root map[string]any, path []string) bool {
	parent, ok := walkPath(root, path[:len(path)-1])
	if !ok {
		return false
	}
	m, ok := parent.(map[string]any)
	if !ok {
		return false
	}
	if _, present := m[path[len(path)-1]]; !present {
		return false
	}
	delete(m, path[len(path)-1])
	return true
}

// listLoc is where a list route's items live.
type listLoc struct {
	route  *paths.Route
	anchor *model.Node
	// owner holds the list: as its children (kids != "") or in its Fields at
	// path.
	owner *model.Node
	kids  string
	path  []string
	// typ filters elements by type.
	typ model.ElementType
}

// kidsOf names the child list each splittable kind lives in.
var kidsOf = map[model.Kind]string{
	model.KindScene:     "scenes",
	model.KindElement:   "body",
	model.KindCharacter: "characters",
}

// locateList finds a list route's items. A list inside analysis on a document
// without analysis is empty rather than missing, with owner nil.
func locateList(t *model.Tree, tg *paths.Target) (*listLoc, error) {
	anchor, err := resolve(t, tg)
	if err != nil {
		return nil, err
	}
	r := tg.Route
	l := &listLoc{route: r, anchor: anchor, typ: model.ElementType(tg.Param(paths.ParamType))}
	if kids, ok := kidsOf[r.Kind]; ok {
		l.owner, l.kids = anchor, kids
		return l, nil
	}
	owner, toks, err := splitPointer(anchor, r.Pointer)
	if err != nil {
		if anchor.Kind == model.KindDocument {
			l.path = strings.Split(strings.TrimPrefix(r.Pointer, "/"), "/")[1:]
			return l, nil
		}
		return nil, err
	}
	l.owner, l.path = owner, toks
	return l, nil
}

// items returns the list's JSON items in document order.
func (l *listLoc) items() ([]any, error) {
	if l.owner == nil {
		return []any{}, nil
	}
	if l.kids != "" {
		out := []any{}
		for _, n := range l.owner.Child(l.kids) {
			if l.typ != "" && n.Type != l.typ {
				continue
			}
			v, err := n.JSON()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	v, _ := walkPath(l.owner.Fields, l.path)
	items, _ := v.([]any)
	if items == nil {
		items = []any{}
	}
	return items, nil
}

// parseIfMatch reads an If-Match header: "14", "c3", W/"14" or *.
func parseIfMatch(h string) (tag string, any bool, ok bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", false, false
	}
	if h == "*" {
		return "", true, true
	}
	h = strings.TrimPrefix(h, "W/")
	return strings.Trim(h, `"`), false, true
}

// precondition checks If-Match against a revision (R-API-11). list compares a
// list insert against the parent's crev, written "c<crev>".
func (m *Manager) precondition(req *Request, rev uint64, list bool) error {
	tag, star, present := parseIfMatch(req.IfMatch)
	if !present {
		if m.o.RequireIfMatch {
			return newErr(http.StatusPreconditionRequired, CodePrecondition, "If-Match is required")
		}
		return nil
	}
	if star {
		return nil
	}
	want := strconv.FormatUint(rev, 10)
	what := "node rev"
	if list {
		want, what = "c"+want, "list crev"
	}
	if tag != want {
		return newErr(http.StatusPreconditionFailed, CodeRevMismatch, "%s is %s, If-Match was %s", what, strings.TrimPrefix(want, "c"), tag)
	}
	return nil
}

// etag formats an ETag for a node or a list.
func etag(rev uint64, list bool) string {
	if list {
		return fmt.Sprintf(`"c%d"`, rev)
	}
	return fmt.Sprintf(`"%d"`, rev)
}

// nodeResponse is the standard response for a node: body, ETag, seq and, when
// the node has text, its text revision (R-API-03).
func nodeResponse(status int, t *model.Tree, revNode *model.Node, body any) *Response {
	r := &Response{Status: status, Body: body, ETag: etag(revNode.Env.Rev, false), Seq: t.Seq()}
	if _, ok := textSig(revNode).(map[string]any); ok || revNode.Kind == model.KindScene || revNode.Kind == model.KindCharacter {
		r.TextRev, r.HasTextRev = revNode.Env.TextRev, true
	}
	return r
}
