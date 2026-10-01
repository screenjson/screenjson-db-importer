package paths

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
)

// Error is a path that can't be served. Status and Code are the HTTP status and
// the error code of SPEC.md 6.2.
type Error struct {
	// Status is the HTTP status: 400, 404, 405 or 410.
	Status int
	// Code is the error code for the response body.
	Code string
	// Message says what went wrong.
	Message string
	// Details lists hints. For an unknown segment it holds the valid children
	// at that point (R-PATH-04).
	Details []string
	// Allow lists the methods a route answers, for the Allow header on 405.
	Allow []string
}

func (e *Error) Error() string { return "paths: " + e.Message }

// The error codes this package produces.
const (
	CodeNotFound         = "not_found"
	CodeBadRequest       = "bad_request"
	CodeGone             = "gone"
	CodeMethodNotAllowed = "method_not_allowed"
)

func notFound(format string, args ...any) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeBadRequest, Message: fmt.Sprintf(format, args...)}
}

// Suffix is what follows a route's own path.
type Suffix uint8

// The suffixes of SPEC.md 6.4.
const (
	// SuffixNone is the route itself.
	SuffixNone Suffix = iota
	// SuffixLang is …/{textField}/{lang}: one language of a language map.
	SuffixLang
	// SuffixSet is …/{setField}: a unique-string array.
	SuffixSet
	// SuffixEmbeddings is …/embeddings: every embedding of the node.
	SuffixEmbeddings
	// SuffixEmbedding is …/embeddings/{model}: one model's embedding.
	SuffixEmbedding
	// SuffixCheckout is …/checkout, a bulk checkout when the route is a list.
	SuffixCheckout
	// SuffixMove is …/move.
	SuffixMove
)

// Target is a parsed request path: the route, its parameters and any suffix.
type Target struct {
	// Route is the table entry matched.
	Route *Route
	// Method is the request method.
	Method Method
	// Params holds the path parameters, unescaped, by name.
	Params map[string]string
	// Suffix is what follows the route's path.
	Suffix Suffix
	// Field is the language-map or set field a SuffixLang or SuffixSet names.
	Field string
	// Wild reports whether {doc} or {scene} is the "-" wildcard.
	Wild bool
	// Template is the pattern including the suffix, for example
	// "/documents/{doc}/scenes/{scene}/elements/{type}/{el}/text/{lang}".
	// Metrics are labelled with it (R-SYS-02).
	Template string

	// anchor and anchorKind are the parameter naming the node Resolve looks up,
	// and the kind it must be. They differ from the route's when a wildcard
	// scene falls back to the document.
	anchor     string
	anchorKind model.Kind
	// anchorPath is the canonical, escaped path of the anchor as requested.
	anchorPath string
	// tail is the escaped rest of the path after the anchor, kept as sent.
	tail string
}

// Param returns a path parameter, or "" if the route has none of that name.
func (t *Target) Param(name string) string { return t.Params[name] }

// Doc returns the {doc} parameter.
func (t *Target) Doc() string { return t.Params[ParamDoc] }

// AnchorID returns the ID of the node the request is anchored on: the value of
// the anchor parameter, or the document's ID when the anchor is the document.
// It is "" when there is none, as with a wildcard document.
func (t *Target) AnchorID() string {
	if t.anchor == "" {
		return ""
	}
	return t.Params[t.anchor]
}

// suffixMethods says which methods each suffix answers (SPEC.md 6.8 to 6.11).
func suffixMethods(s Suffix, list bool) Method {
	switch s {
	case SuffixLang:
		return GET | PUT | DELETE
	case SuffixSet:
		return GET | PUT | POST
	case SuffixEmbeddings:
		return GET
	case SuffixEmbedding:
		return GET | PUT | DELETE
	case SuffixCheckout:
		if list {
			return POST
		}
		return POST | PUT | DELETE
	case SuffixMove:
		return POST
	}
	return 0
}

// Table is the compiled route table.
type Table struct {
	routes []*Route
	byName map[string]*Route
	root   *trieNode
}

// trieNode is one path segment in the compiled table.
type trieNode struct {
	lit    map[string]*trieNode
	params []paramChild
	route  *Route
}

type paramChild struct {
	name string
	node *trieNode
}

// Roots lists the first path segment of every route, such as "documents"
// and "status". A request whose path starts with one of them is an API call;
// anything else belongs to the web app.
func (tb *Table) Roots() map[string]bool {
	out := map[string]bool{}
	for _, r := range tb.routes {
		out[r.segs[0]] = true
	}
	return out
}

// NewTable compiles the route table. It fails if the table is inconsistent: two
// routes on one pattern, an unknown parameter, or a child segment that would
// shadow a suffix.
func NewTable() (*Table, error) {
	tb := &Table{routes: table(), byName: map[string]*Route{}, root: newTrieNode()}
	for _, r := range tb.routes {
		if _, dup := tb.byName[r.Name]; dup {
			return nil, fmt.Errorf("paths: route name %q used twice", r.Name)
		}
		tb.byName[r.Name] = r
		r.segs = strings.Split(strings.TrimPrefix(r.Pattern, "/"), "/")
		if err := tb.insert(r); err != nil {
			return nil, err
		}
	}
	for _, r := range tb.routes {
		r.owned = ownedFields(r, tb.routes)
	}
	if err := checkShadowing(tb.root); err != nil {
		return nil, err
	}
	return tb, nil
}

func newTrieNode() *trieNode { return &trieNode{lit: map[string]*trieNode{}} }

func (tb *Table) insert(r *Route) error {
	node := tb.root
	for _, seg := range r.segs {
		name, isParam := paramName(seg)
		if !isParam {
			next, ok := node.lit[seg]
			if !ok {
				next = newTrieNode()
				node.lit[seg] = next
			}
			node = next
			continue
		}
		if !knownParam(name) {
			return fmt.Errorf("paths: route %s: unknown parameter {%s}", r.Name, name)
		}
		var next *trieNode
		for _, pc := range node.params {
			if pc.name == name {
				next = pc.node
			}
		}
		if next == nil {
			next = newTrieNode()
			node.params = append(node.params, paramChild{name: name, node: next})
		}
		node = next
	}
	if node.route != nil {
		return fmt.Errorf("paths: routes %s and %s share pattern %s", node.route.Name, r.Name, r.Pattern)
	}
	node.route = r
	return nil
}

// ownedFields works out Route.Owned: the set fields, plus the first field of
// every other route's pointer that sits inside this route's object.
func ownedFields(r *Route, all []*Route) []string {
	if !r.IsNode() {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	prefix := r.Pointer + "/"
	for _, o := range all {
		if o == r || o.Anchor != r.Anchor || o.AnchorKind != r.AnchorKind || o.Pointer == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(o.Pointer, prefix); ok {
			first, _, _ := strings.Cut(rest, "/")
			add(first)
		}
	}
	for _, f := range r.SetFields {
		add(f)
	}
	sort.Strings(out)
	return out
}

// checkShadowing makes sure no literal child segment has the same name as a
// suffix its parent's route accepts, since the child would always win.
func checkShadowing(n *trieNode) error {
	if n.route != nil {
		for _, s := range suffixNames(n.route) {
			if _, clash := n.lit[s]; clash {
				return fmt.Errorf("paths: route %s: child segment %q shadows a suffix", n.route.Name, s)
			}
		}
	}
	for _, next := range n.lit {
		if err := checkShadowing(next); err != nil {
			return err
		}
	}
	for _, pc := range n.params {
		if err := checkShadowing(pc.node); err != nil {
			return err
		}
	}
	return nil
}

// suffixNames lists the first segment of every suffix a route accepts.
func suffixNames(r *Route) []string {
	var out []string
	if r.Allows&CanLang != 0 {
		out = append(out, r.LangFields...)
	}
	if r.Allows&CanSet != 0 {
		out = append(out, r.SetFields...)
	}
	if r.Allows&CanEmbed != 0 {
		out = append(out, "embeddings")
	}
	if r.Allows&CanCheckout != 0 {
		out = append(out, "checkout")
	}
	if r.Allows&CanMove != 0 {
		out = append(out, "move")
	}
	return out
}

// Routes returns the routes in table order.
func (tb *Table) Routes() []*Route {
	out := make([]*Route, len(tb.routes))
	copy(out, tb.routes)
	return out
}

// Route returns a route by name, or nil.
func (tb *Table) Route(name string) *Route { return tb.byName[name] }

// Template is one path the API answers: a route with or without a suffix. The
// OpenAPI document has one path item per Template (R-SYS-01).
type Template struct {
	// Pattern is the full path pattern.
	Pattern string
	// Methods are the methods answered there.
	Methods Method
	// Route is the table entry.
	Route *Route
	// Suffix is the suffix Pattern adds to Route.Pattern.
	Suffix Suffix
}

// Templates expands every route with every suffix it accepts.
func (tb *Table) Templates() []Template {
	var out []Template
	for _, r := range tb.routes {
		out = append(out, Template{Pattern: r.Pattern, Methods: r.Methods, Route: r})
		add := func(s Suffix, tail string) {
			out = append(out, Template{Pattern: r.Pattern + tail, Methods: suffixMethods(s, r.List), Route: r, Suffix: s})
		}
		if r.Allows&CanLang != 0 {
			for _, f := range r.LangFields {
				add(SuffixLang, "/"+f+"/{lang}")
			}
		}
		if r.Allows&CanSet != 0 {
			for _, f := range r.SetFields {
				add(SuffixSet, "/"+f)
			}
		}
		if r.Allows&CanEmbed != 0 {
			add(SuffixEmbeddings, "/embeddings")
			add(SuffixEmbedding, "/embeddings/{model}")
		}
		if r.Allows&CanCheckout != 0 {
			add(SuffixCheckout, "/checkout")
		}
		if r.Allows&CanMove != 0 {
			add(SuffixMove, "/move")
		}
	}
	return out
}

// Parse matches a request against the table. path must be the escaped path
// (url.URL.EscapedPath), so that an embedding model name holding an encoded "/"
// stays one segment (R-EMB-01).
//
// It answers 404 for an unknown path, listing the valid segments at the point
// matching stopped (R-PATH-04); 405 for a method the path doesn't answer; and
// 400 for a malformed escape or a wildcard where one isn't allowed (R-PATH-03).
// It does not look at any document: Resolve does that.
func (tb *Table) Parse(method, path string) (*Target, error) {
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, notFound("path %q does not start with /", path)
	}
	raw := strings.Split(path[1:], "/")
	segs := make([]string, len(raw))
	for i, s := range raw {
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, badRequest("path segment %q is not validly escaped", s)
		}
		segs[i] = u
	}

	tg := &Target{Method: ParseMethod(method), Params: map[string]string{}}
	node := tb.root
	i := 0
walk:
	for ; i < len(segs); i++ {
		if node.route != nil && node.route.Redirect {
			break
		}
		s := segs[i]
		if next, ok := node.lit[s]; ok {
			node = next
			continue
		}
		for _, pc := range node.params {
			if validParam(pc.name, s) {
				tg.Params[pc.name] = s
				if s == Wildcard {
					tg.Wild = true
				}
				node = pc.node
				continue walk
			}
		}
		break
	}

	r := node.route
	if r == nil {
		return nil, unknownSegment(node, path, segs, i)
	}
	tg.Route = r
	tg.Template = r.Pattern
	rest := segs[i:]

	allowed := r.Methods
	switch {
	case r.Redirect:
		// Everything after the anchor is carried over to the redirect as sent.
	case len(rest) > 0:
		if !tg.parseSuffix(rest) {
			return nil, unknownSegment(node, path, segs, i)
		}
		allowed = suffixMethods(tg.Suffix, r.List)
	}

	if !allowed.Has(tg.Method) {
		return nil, &Error{
			Status:  http.StatusMethodNotAllowed,
			Code:    CodeMethodNotAllowed,
			Message: fmt.Sprintf("%s is not allowed on %s", method, tg.Template),
			Allow:   allowed.Names(),
		}
	}

	if tg.Wild {
		listRead := r.List && tg.Suffix == SuffixNone && tg.Method == GET
		bulk := r.List && tg.Suffix == SuffixCheckout
		if !listRead && !bulk {
			return nil, badRequest("the %q wildcard may only stand for a document or scene on a GET of a list or a bulk checkout", Wildcard)
		}
	}

	tg.setAnchor(raw)
	return tg, nil
}

// parseSuffix matches what follows the route's own path.
func (tg *Target) parseSuffix(rest []string) bool {
	r := tg.Route
	switch len(rest) {
	case 1:
		s := rest[0]
		switch {
		case s == "checkout" && r.Allows&CanCheckout != 0:
			tg.Suffix = SuffixCheckout
		case s == "move" && r.Allows&CanMove != 0:
			tg.Suffix = SuffixMove
		case s == "embeddings" && r.Allows&CanEmbed != 0:
			tg.Suffix = SuffixEmbeddings
		case r.Allows&CanSet != 0 && contains(r.SetFields, s):
			tg.Suffix, tg.Field = SuffixSet, s
		default:
			return false
		}
		tg.Template += "/" + s
		return true
	case 2:
		switch {
		case rest[0] == "embeddings" && r.Allows&CanEmbed != 0 && rest[1] != "":
			tg.Suffix = SuffixEmbedding
			tg.Params[ParamModel] = rest[1]
			tg.Template += "/embeddings/{model}"
			return true
		case r.Allows&CanLang != 0 && contains(r.LangFields, rest[0]) && validLang(rest[1]):
			// A cue has no text map (SPEC.md 3.3), so no …/text/{lang} either.
			if r.Kind == model.KindElement && rest[0] == "text" &&
				tg.Params[ParamType] == string(model.TypeCharacter) {
				return false
			}
			tg.Suffix, tg.Field = SuffixLang, rest[0]
			tg.Params[ParamLang] = rest[1]
			tg.Template += "/" + rest[0] + "/{lang}"
			return true
		}
	}
	return false
}

// setAnchor records the node Resolve will look up and the canonical path it was
// requested at. raw is the escaped path split into segments.
func (tg *Target) setAnchor(raw []string) {
	r := tg.Route
	if r.Anchor == "" {
		return
	}
	tg.anchor, tg.anchorKind = r.Anchor, r.AnchorKind
	if tg.Params[tg.anchor] == Wildcard {
		// Only a scene can be a wildcard anchor, and only on a list: the
		// document then anchors the request instead.
		tg.anchor, tg.anchorKind = ParamDoc, model.KindDocument
	}
	if tg.Params[tg.anchor] == Wildcard {
		tg.anchor, tg.anchorKind = "", ""
		return
	}

	var b strings.Builder
	for i, seg := range r.segs {
		name, isParam := paramName(seg)
		b.WriteByte('/')
		if !isParam {
			b.WriteString(seg)
			continue
		}
		b.WriteString(url.PathEscape(tg.Params[name]))
		if name == tg.anchor {
			tg.anchorPath = b.String()
			if i+1 < len(raw) {
				tg.tail = "/" + strings.Join(raw[i+1:], "/")
			}
			return
		}
	}
}

// unknownSegment builds the 404 for a path that stopped matching at segs[i],
// listing what would have matched there (R-PATH-04).
func unknownSegment(node *trieNode, path string, segs []string, i int) *Error {
	var hints []string
	for lit := range node.lit {
		hints = append(hints, lit)
	}
	for _, pc := range node.params {
		hints = append(hints, "{"+pc.name+"}")
	}
	if r := node.route; r != nil {
		if r.Allows&CanLang != 0 {
			for _, f := range r.LangFields {
				hints = append(hints, f+"/{lang}")
			}
		}
		if r.Allows&CanSet != 0 {
			hints = append(hints, r.SetFields...)
		}
		if r.Allows&CanEmbed != 0 {
			hints = append(hints, "embeddings", "embeddings/{model}")
		}
		if r.Allows&CanCheckout != 0 {
			hints = append(hints, "checkout")
		}
		if r.Allows&CanMove != 0 {
			hints = append(hints, "move")
		}
	}
	sort.Strings(hints)

	e := notFound("no route for %s", path)
	if i < len(segs) {
		e.Message = fmt.Sprintf("no route for %s: unknown segment %q", path, segs[i])
	}
	if len(hints) > 0 {
		e.Details = make([]string, 0, len(hints))
		for _, h := range hints {
			e.Details = append(e.Details, "valid here: "+h)
		}
	}
	return e
}

// paramName reports whether a pattern segment is a {param}, and its name.
func paramName(seg string) (string, bool) {
	if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
		return seg[1 : len(seg)-1], true
	}
	return "", false
}

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	// langPattern is the schema's text-map key pattern.
	langPattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
)

// knownParam reports whether validParam has a rule for a parameter name.
func knownParam(name string) bool {
	switch name {
	case ParamDoc, ParamScene, ParamType, ParamElement, ParamKey, ParamSlug,
		ParamRegistration, ParamNote, ParamRevision, ParamID, ParamUUID,
		ParamName, ParamLabel, ParamModel, ParamFile, ParamJob:
		return true
	}
	return false
}

// validParam checks a path parameter's value. Where two parameters sit at one
// point in the table ({type} and {el} after /elements) their rules are
// disjoint, so the order they are tried in doesn't matter.
func validParam(name, v string) bool {
	switch name {
	case ParamDoc, ParamScene:
		return v == Wildcard || uuidPattern.MatchString(v)
	case ParamType:
		return model.ValidElementType(model.ElementType(v))
	case ParamElement, ParamKey, ParamNote, ParamRevision, ParamID, ParamUUID:
		return uuidPattern.MatchString(v)
	case ParamSlug:
		return slugPattern.MatchString(v)
	case ParamRegistration:
		_, _, ok := SplitRegistrationKey(v)
		return ok
	case ParamName, ParamLabel, ParamFile:
		return v != ""
	case ParamJob:
		return len(v) <= 2048 && jobPattern.MatchString(v)
	}
	return false
}

// jobPattern matches a Greenlight job ID as the server writes it: base64url.
var jobPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)

func validLang(v string) bool { return langPattern.MatchString(v) }

// RegistrationKey is how a registration is addressed: "{authority}~{id}". Its
// id is an authority's own string rather than a UUID, so it is only unique
// together with the authority.
func RegistrationKey(authority, id string) string { return authority + "~" + id }

// SplitRegistrationKey reverses RegistrationKey. The split is at the first "~",
// so an authority can't contain one; an id can.
func SplitRegistrationKey(key string) (authority, id string, ok bool) {
	authority, id, found := strings.Cut(key, "~")
	if !found || authority == "" || id == "" {
		return "", "", false
	}
	return authority, id, true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
