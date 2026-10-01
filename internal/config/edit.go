package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source says where a setting's value came from.
type Source string

// Sources, weakest first. A setting changed through PATCH /config is "file"
// once written there, or "api" when there was no file to write: then it
// lasts until the server stops.
const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
	SourceAPI     Source = "api"
)

// File is the YAML file the configuration was read from, or "".
func (c *Config) File() string { return c.file }

// SetFile names the YAML file edits are written to, for a configuration
// built without Load (tests).
func (c *Config) SetFile(path string) { c.file = path }

// Source says where a setting's value came from.
func (c *Config) Source(path string) Source {
	if s, ok := c.sources[path]; ok {
		return s
	}
	return SourceDefault
}

// Writable reports whether edits can be written back to the YAML file: there
// is one, and this process may write it (or create it).
func (c *Config) Writable() bool {
	if c.file == "" {
		return false
	}
	f, err := os.OpenFile(c.file, os.O_WRONLY, 0)
	if err == nil {
		f.Close()
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	probe, err := os.CreateTemp(filepath.Dir(c.file), ".screenjson-probe-*")
	if err != nil {
		return false
	}
	probe.Close()
	os.Remove(probe.Name())
	return true
}

// filePaths lists the setting paths a YAML file sets: every key path in it,
// such as "greenlight" and "greenlight.servers".
func filePaths(raw []byte) []string {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	var out []string
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			p := prefix + n.Content[i].Value
			out = append(out, p)
			walk(n.Content[i+1], p+".")
		}
	}
	walk(doc.Content[0], "")
	return out
}

// Setting is one setting as GET /config shows it.
type Setting struct {
	Path string `json:"path"`
	Env  string `json:"env,omitempty"`
	Doc  string `json:"doc"`
	// Type is bool, int, string, list (of strings), size, duration, layout
	// or entries (a list of objects, YAML only).
	Type  string `json:"type"`
	Value any    `json:"value"`
	// Choices are the allowed values, for a setting that takes one of a list.
	Choices []string `json:"choices,omitempty"`
	Secret  bool     `json:"secret,omitempty"`
	Source  Source   `json:"source"`
	// Live is true when a running server applies a change at once.
	Live bool `json:"live"`
	// Editable is true when PATCH /config may change it; Locked says why
	// not otherwise.
	Editable bool   `json:"editable"`
	Locked   string `json:"locked,omitempty"`
}

func (f Field) kind() string {
	switch {
	case f.typ == sizeType:
		return "size"
	case f.typ == durationType:
		return "duration"
	case f.typ == layoutType:
		return "layout"
	case f.typ.Kind() == reflect.Bool, f.typ.Kind() == reflect.Pointer && f.typ.Elem().Kind() == reflect.Bool:
		return "bool"
	case f.typ.Kind() == reflect.Int:
		return "int"
	case f.typ.Kind() == reflect.Slice && f.typ.Elem().Kind() == reflect.String:
		return "list"
	case f.typ.Kind() == reflect.Slice:
		return "entries"
	}
	return "string"
}

// locked says why PATCH /config may not change a setting, or "".
func (c *Config) locked(f Field) string {
	switch {
	case f.Fixed && f.Path == "admin.key":
		return "set in the environment or the file only"
	case f.Fixed:
		return "changing it needs the relayout command (docs/CONFIG.md)"
	case f.kind() == "entries":
		return "a list of entries: edit the YAML file"
	case c.Source(f.Path) == SourceEnv:
		return "set by the environment (" + f.Env + "), which overrides the file"
	case c.Source(f.Path) == SourceFlag:
		return "set by a command-line flag, which overrides the file"
	}
	return ""
}

// jsonValue is the setting's value for GET /config: a bool, a number, a list
// of strings or text, with secrets masked.
func (f Field) jsonValue(c *Config) any {
	v := reflect.ValueOf(c).Elem().FieldByIndex(f.index)
	switch f.kind() {
	case "bool":
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return nil
			}
			return v.Elem().Bool()
		}
		return v.Bool()
	case "int":
		return v.Int()
	case "list":
		return append([]string{}, v.Interface().([]string)...)
	}
	s := f.value(c)
	if f.Secret {
		s = Mask(s)
	}
	return s
}

// Settings lists every setting with its value, where it came from and
// whether it can be changed.
func (c *Config) Settings() []Setting {
	var out []Setting
	for _, f := range Fields() {
		lock := c.locked(f)
		out = append(out, Setting{Path: f.Path, Env: f.Env, Doc: f.Doc, Type: f.kind(), Value: f.jsonValue(c), Choices: Choices[f.Path],
			Secret: f.Secret, Source: c.Source(f.Path), Live: f.Live, Editable: lock == "", Locked: lock})
	}
	return out
}

// FieldError is a change PATCH /config refused, by setting.
type FieldError map[string]string

func (e FieldError) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + e[k]
	}
	return "config: " + strings.Join(parts, "; ")
}

// text turns a JSON value into the text form settings are parsed from.
func text(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		if x != float64(int64(x)) {
			return "", fmt.Errorf("%v is not a whole number", x)
		}
		return strconv.FormatInt(int64(x), 10), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return "", errors.New("a list must hold text")
			}
			if strings.Contains(s, ",") {
				return "", fmt.Errorf("%q: list items can't contain commas", s)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), nil
	case nil:
		return "", errors.New("give a value")
	}
	return "", fmt.Errorf("unsupported value %v", v)
}

// With returns a copy of the configuration with the changes made, and the
// paths that actually changed. Values are JSON: text, true/false, numbers or
// lists of text. A secret sent back as its masked form is left as it was.
// Every refused change is reported, and the result must pass Validate.
func (c *Config) With(changes map[string]any) (*Config, []string, error) {
	next := *c // Field.set replaces values whole, so a shallow copy is enough
	next.sources = make(map[string]Source, len(c.sources))
	for k, v := range c.sources {
		next.sources[k] = v
	}
	bad := FieldError{}
	var changed []string
	for path, v := range changes {
		f, ok := FieldByPath(path)
		if !ok {
			bad[path] = "no such setting"
			continue
		}
		if why := c.locked(f); why != "" {
			bad[path] = why
			continue
		}
		s, err := text(v)
		if err != nil {
			bad[path] = err.Error()
			continue
		}
		if f.Secret && s == Mask(f.value(c)) {
			continue
		}
		before := f.value(&next)
		if err := f.set(&next, s); err != nil {
			bad[path] = err.Error()
			continue
		}
		if f.value(&next) != before {
			changed = append(changed, path)
		}
	}
	if len(bad) > 0 {
		return nil, nil, bad
	}
	if err := next.Validate(); err != nil {
		return nil, nil, err
	}
	sort.Strings(changed)
	return &next, changed, nil
}

// MarkEdited records where edited settings now live: the file when they were
// written there, else only this running server.
func (c *Config) MarkEdited(paths []string, written bool) {
	src := SourceAPI
	if written {
		src = SourceFile
	}
	for _, p := range paths {
		c.sources[p] = src
	}
}

// WriteFile writes the named settings' values into the YAML file, keeping
// everything else in it, comments included. A missing file is created.
func (c *Config) WriteFile(paths []string) error {
	if c.file == "" {
		return errors.New("config: no YAML file to write to")
	}
	raw, err := os.ReadFile(c.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: read %s: %w", c.file, err)
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("config: %s: %w", c.file, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	for _, path := range paths {
		f, ok := FieldByPath(path)
		if !ok {
			return fmt.Errorf("config: no setting %q", path)
		}
		v := reflect.ValueOf(c).Elem().FieldByIndex(f.index)
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		var val yaml.Node
		if err := val.Encode(v.Interface()); err != nil {
			return fmt.Errorf("config: %s: %w", path, err)
		}
		setPath(doc.Content[0], strings.Split(path, "."), &val)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	enc.Close()
	return writeAtomic(c.file, buf.Bytes())
}

// setPath puts val at the key path in a mapping, making mappings on the way
// and keeping the comments of a value it replaces.
func setPath(m *yaml.Node, keys []string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != keys[0] {
			continue
		}
		if len(keys) == 1 {
			old := m.Content[i+1]
			val.HeadComment, val.LineComment, val.FootComment = old.HeadComment, old.LineComment, old.FootComment
			m.Content[i+1] = val
			return
		}
		if m.Content[i+1].Kind != yaml.MappingNode {
			m.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		setPath(m.Content[i+1], keys[1:], val)
		return
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: keys[0]}
	if len(keys) == 1 {
		m.Content = append(m.Content, key, val)
		return
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, key, child)
	setPath(child, keys[1:], val)
}

// writeAtomic replaces the file through a temporary one beside it. A file
// that can't be replaced that way, such as one bind-mounted into a
// container, is rewritten in place.
func writeAtomic(path string, b []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".screenjson-*.yaml")
	if err == nil {
		_, werr := tmp.Write(b)
		cerr := tmp.Close()
		if werr == nil && cerr == nil && os.Chmod(tmp.Name(), mode) == nil && os.Rename(tmp.Name(), path) == nil {
			return nil
		}
		os.Remove(tmp.Name())
	}
	if err := os.WriteFile(path, b, mode); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}
