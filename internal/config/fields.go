package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Field is one setting.
type Field struct {
	// Path is its YAML path, such as "storage.driver".
	Path string
	// Env is its environment name, or "" for list settings that are YAML only.
	Env string
	// Doc says what it does.
	Doc string
	// Secret marks values masked by config check.
	Secret bool
	// Live marks settings a running server applies when they are changed
	// through PATCH /config; the rest take effect at the next start.
	Live bool
	// Fixed marks settings PATCH /config never changes (tag edit:"false").
	Fixed bool
	// index locates the field in Config for reflect.Value.FieldByIndex.
	index []int
	typ   reflect.Type
}

// EnvName turns a YAML path into its environment name (R-CFG-03).
func EnvName(path string) string {
	return "SCREENJSON_" + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

var (
	sizeType     = reflect.TypeOf(Size(0))
	durationType = reflect.TypeOf(Duration(0))
	layoutType   = reflect.TypeOf(LayoutSetting{})
)

// Fields lists every setting in declaration order.
func Fields() []Field {
	var out []Field
	var walk func(t reflect.Type, prefix string, index []int)
	walk = func(t reflect.Type, prefix string, index []int) {
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			name, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			path := prefix + name
			idx := append(append([]int{}, index...), i)
			ft := sf.Type
			if ft.Kind() == reflect.Struct && ft != layoutType {
				walk(ft, path+".", idx)
				continue
			}
			f := Field{Path: path, Env: EnvName(path), Doc: sf.Tag.Get("doc"),
				Secret: sf.Tag.Get("secret") == "true", Live: sf.Tag.Get("live") == "true",
				Fixed: sf.Tag.Get("edit") == "false", index: idx, typ: ft}
			if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct {
				f.Env = ""
			}
			out = append(out, f)
		}
	}
	walk(reflect.TypeOf(Config{}), "", nil)
	return out
}

// FieldByPath finds a setting by its YAML path.
func FieldByPath(path string) (Field, bool) {
	for _, f := range Fields() {
		if f.Path == path {
			return f, true
		}
	}
	return Field{}, false
}

// set parses a string into the field.
func (f Field) set(c *Config, s string) error {
	v := reflect.ValueOf(c).Elem().FieldByIndex(f.index)
	switch {
	case f.typ == sizeType:
		n, err := ParseSize(s)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(n))
	case f.typ == durationType:
		d, err := ParseDuration(s)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(d))
	case f.typ == layoutType:
		var l LayoutSetting
		if err := l.Set(s); err != nil {
			return err
		}
		v.Set(reflect.ValueOf(l))
	case f.typ.Kind() == reflect.String:
		v.SetString(s)
	case f.typ.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("%q is not true or false", s)
		}
		v.SetBool(b)
	case f.typ.Kind() == reflect.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("%q is not a whole number", s)
		}
		v.SetInt(int64(n))
	case f.typ.Kind() == reflect.Pointer && f.typ.Elem().Kind() == reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("%q is not true or false", s)
		}
		v.Set(reflect.ValueOf(&b))
	case f.typ.Kind() == reflect.Slice && f.typ.Elem().Kind() == reflect.String:
		var list []string
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				list = append(list, p)
			}
		}
		v.Set(reflect.ValueOf(list))
	default:
		return fmt.Errorf("%s can only be set in the YAML file", f.Path)
	}
	return nil
}

// value returns the field's current value as YAML-style text.
func (f Field) value(c *Config) string {
	v := reflect.ValueOf(c).Elem().FieldByIndex(f.index)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "(driver default)"
		}
		v = v.Elem()
	}
	if s, ok := v.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	if f.typ == layoutType {
		l := v.Interface().(LayoutSetting)
		if l.Preset != "" {
			return l.Preset
		}
		return "(custom)"
	}
	if v.Kind() == reflect.Slice {
		if v.Len() == 0 {
			return "[]"
		}
		if v.Type().Elem().Kind() == reflect.Struct {
			return fmt.Sprintf("(%d entries)", v.Len())
		}
	}
	return fmt.Sprint(v.Interface())
}

// Masked writes the configuration as YAML with every secret replaced, for
// config check (R-CFG-05).
func (c *Config) Masked() (string, error) {
	cp := *c
	cp.Webhooks = append([]Webhook(nil), c.Webhooks...)
	for i := range cp.Webhooks {
		cp.Webhooks[i].Secret = Mask(cp.Webhooks[i].Secret)
	}
	cp.Git.Credentials = append([]GitCredential(nil), c.Git.Credentials...)
	for i := range cp.Git.Credentials {
		cp.Git.Credentials[i].Token = Mask(cp.Git.Credentials[i].Token)
	}
	for _, f := range Fields() {
		if !f.Secret || f.typ.Kind() != reflect.String {
			continue
		}
		v := reflect.ValueOf(&cp).Elem().FieldByIndex(f.index)
		v.SetString(Mask(v.String()))
	}
	b, err := yaml.Marshal(&cp)
	return string(b), err
}

// MaskedMap is Masked as a map, for GET /status.
func (c *Config) MaskedMap() (map[string]any, error) {
	s, err := c.Masked()
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, yaml.Unmarshal([]byte(s), &out)
}

// Mask shows a secret's first 5 characters and stars the rest, so a key can
// be recognised without being given away. Shorter secrets are all stars.
func Mask(v string) string {
	n := utf8.RuneCountInString(v)
	if n == 0 {
		return ""
	}
	if n <= 5 {
		return strings.Repeat("*", n)
	}
	r := []rune(v)
	return string(r[:5]) + strings.Repeat("*", n-5)
}

// Reference writes docs/CONFIG.md: every setting with its environment name,
// default and meaning (R-CFG-07), and the CLI aliases (12.1).
func Reference() string {
	def := Default()
	var b strings.Builder
	b.WriteString("# Configuration\n\n")
	b.WriteString("Generated from `internal/config` by `go run ./internal/config/cmd/configdoc`; a test keeps it in step with the code.\n\n")
	b.WriteString("Settings come from, highest precedence first: command-line flags (`--set path=value`), environment variables, ")
	b.WriteString("the YAML file (`--config` or `SCREENJSON_CONFIG`), and the defaults below. ")
	b.WriteString("`${VAR}` and `${VAR:-default}` in the YAML file are filled from the environment.\n\n")
	b.WriteString("| Setting | Environment | Default | Meaning |\n|---|---|---|---|\n")
	for _, f := range Fields() {
		env := "`" + f.Env + "`"
		if f.Env == "" {
			env = "YAML only"
		}
		dv := f.value(def)
		if dv == "" {
			dv = "(empty)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s |\n", f.Path, env, dv, f.Doc)
	}
	b.WriteString("\n## Webhook entries\n\nEach entry under `webhooks` has `name`, `url`, `on` (a list of events), and optionally ")
	b.WriteString("`secret` (signs deliveries with HMAC-SHA256), `kinds` and `types` (filters).\n")
	b.WriteString("\n## CLI aliases\n\nThe `screenjson` CLI's own variables are accepted with the same meaning. The server's names win when both are set.\n\n")
	b.WriteString("| CLI variable | Setting |\n|---|---|\n")
	keys := make([]string, 0, len(Aliases))
	for k := range Aliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", k, Aliases[k])
	}
	b.WriteString("| `SCREENJSON_DB_HOST`, `_PORT`, `_USER`, `_PASS` | build `storage.url` when it is not set |\n")
	return b.String()
}
