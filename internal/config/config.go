// Package config loads the server's settings (SPEC.md 12): built-in defaults,
// then the YAML file, then environment variables, then command-line flags,
// each overriding the one before (R-CFG-01).
//
// Every setting is a field of Config. Its YAML path gives its environment name
// (R-CFG-03), and its doc tag is its entry in docs/CONFIG.md, which is
// generated from the struct and checked against it (R-CFG-07).
package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/screenjson/screenjson-db-importer/internal/layout"
)

// Config is every setting.
type Config struct {
	Mode    string `yaml:"mode" doc:"full serves the web UI and the API; headless serves only the API (15.5)."`
	DataDir string `yaml:"data_dir" doc:"Where the state store (state.db) and Git work copies live."`
	Log     struct {
		Level string `yaml:"level" live:"true" doc:"debug, info, warn or error."`
	} `yaml:"log"`
	Admin     Admin     `yaml:"admin"`
	HTTP      HTTP      `yaml:"http"`
	Storage   Storage   `yaml:"storage"`
	Cache     Cache     `yaml:"cache"`
	Queue     Queue     `yaml:"queue"`
	Checkouts Checkouts `yaml:"checkouts"`
	Numbering struct {
		Default string `yaml:"default" doc:"Scene numbering when a document sets none in meta: none, auto or frozen (7.4)."`
	} `yaml:"numbering"`
	Events     Events     `yaml:"events"`
	Webhooks   []Webhook  `yaml:"webhooks" doc:"Webhook receivers (section 9). YAML only."`
	Git        Git        `yaml:"git"`
	Blob       Blob       `yaml:"blob"`
	Greenlight Greenlight `yaml:"greenlight"`

	// file is the YAML file the configuration was read from, if any, and
	// sources says where each setting's value came from.
	file    string
	sources map[string]Source
}

// Admin gates changes to the server's own configuration.
type Admin struct {
	Key string `yaml:"key" secret:"true" edit:"false" doc:"Allows editing the configuration through PATCH /config (and the Status page), sent as the X-Admin-Key header. Empty means the configuration is read-only there. Set it in the environment or the file; it can't be changed through the API."`
}

// HTTP is the listener.
type HTTP struct {
	Addr           string   `yaml:"addr" doc:"Listen address. The container image sets 0.0.0.0:8080."`
	CORSOrigins    []string `yaml:"cors_origins" doc:"Origins allowed by CORS, for headless mode with another UI. Comma-separated in the environment."`
	RequireToken   bool     `yaml:"require_token" live:"true" doc:"Refuse requests without a token with 401 (R-TOK-04)."`
	RequireIfMatch bool     `yaml:"require_if_match" doc:"Refuse writes without If-Match with 428."`
	MaxBody        Size     `yaml:"max_body" live:"true" doc:"Largest request body; larger ones get 413."`
	MCP            bool     `yaml:"mcp" doc:"Serve the Model Context Protocol endpoint at /mcp: the API as tools for LLMs, with the caller's token."`
}

// Storage is the database and layout.
type Storage struct {
	Driver            string        `yaml:"driver" doc:"memory, mongo, postgres, elastic, chroma, weaviate or pinecone."`
	URL               string        `yaml:"url" secret:"true" doc:"Connection URL for the driver."`
	Database          string        `yaml:"database" doc:"Database name, where the driver has one."`
	Transactions      *bool         `yaml:"transactions" doc:"Wrap multi-record writes in a transaction. Defaults to true on mongo and postgres, and is unavailable elsewhere."`
	EnvelopeField     string        `yaml:"envelope_field" edit:"false" doc:"Name of the envelope field on mongo and elastic records."`
	AllowLargeRecords bool          `yaml:"allow_large_records" doc:"Let chroma, weaviate and pinecone store a whole screenplay in one record (R-DRV-07)."`
	Layout            LayoutSetting `yaml:"layout" edit:"false" doc:"A preset name (whole, scenes, elements) or a map of levels. In the environment, a preset name or a JSON layout."`
	Elastic           struct {
		Refresh string `yaml:"refresh" doc:"Elastic refresh policy for writes: wait_for, true or false."`
	} `yaml:"elastic"`
	Chroma struct {
		Tenant   string `yaml:"tenant" doc:"Chroma tenant."`
		Database string `yaml:"database" doc:"Chroma database."`
	} `yaml:"chroma"`
	Weaviate struct {
		ClassPrefix string `yaml:"class_prefix" doc:"Prefix for Weaviate class names."`
	} `yaml:"weaviate"`
	Pinecone struct {
		APIKey    string `yaml:"api_key" secret:"true" doc:"Pinecone API key."`
		Namespace string `yaml:"namespace" doc:"Optional prefix for the namespaces used for server collections."`
	} `yaml:"pinecone"`
}

// Cache bounds the document cache (4.5).
type Cache struct {
	MaxDocuments int  `yaml:"max_documents" doc:"Most documents held in memory."`
	MaxBytes     Size `yaml:"max_bytes" doc:"Most bytes of documents held in memory."`
}

// Queue sizes the per-document write queues.
type Queue struct {
	Length int `yaml:"length" doc:"Operations a document's write queue holds before answering 429 (R-ARCH-04)."`
}

// Checkouts sets checkout lifetimes (R-CO-01).
type Checkouts struct {
	DefaultTTL Duration `yaml:"default_ttl" doc:"Checkout lifetime when none is asked for."`
	MaxTTL     Duration `yaml:"max_ttl" doc:"Longest checkout lifetime allowed."`
}

// Events picks the live event backend (section 8).
type Events struct {
	Backend     string   `yaml:"backend" doc:"embedded (Centrifuge inside the server) or centrifugo (an external server)."`
	HistorySize int      `yaml:"history_size" doc:"Events kept per document channel for recovery."`
	HistoryTTL  Duration `yaml:"history_ttl" doc:"How long events are kept for recovery."`
	Centrifugo  struct {
		APIURL    string `yaml:"api_url" doc:"Centrifugo HTTP API URL."`
		APIKey    string `yaml:"api_key" secret:"true" doc:"Centrifugo API key."`
		ClientURL string `yaml:"client_url" doc:"Websocket URL browsers connect to."`
	} `yaml:"centrifugo"`
}

// Webhook is one receiver (section 9).
type Webhook struct {
	Name   string   `yaml:"name"`
	URL    string   `yaml:"url"`
	On     []string `yaml:"on"`
	Secret string   `yaml:"secret" secret:"true"`
	Kinds  []string `yaml:"kinds"`
	Types  []string `yaml:"types"`
}

// Git is the Git export (section 10).
type Git struct {
	Enabled bool   `yaml:"enabled" doc:"Turn Git export on."`
	Mode    string `yaml:"mode" doc:"repo-per-doc or mono."`
	Workdir string `yaml:"workdir" doc:"Where local work copies live. Defaults to data_dir/git."`
	Remote  string `yaml:"remote" doc:"Remote URL; {doc} and {slug} are filled in per document in repo-per-doc mode."`
	Branch  string `yaml:"branch" doc:"Branch to commit to."`
	Push    bool   `yaml:"push" doc:"Push after each commit."`
	Author  struct {
		Name  string `yaml:"name" doc:"Commit author name."`
		Email string `yaml:"email" doc:"Commit author email."`
	} `yaml:"author"`
	IncludeAnalysis bool     `yaml:"include_analysis" doc:"Include analysis in committed files."`
	AutoCommitIdle  Duration `yaml:"auto_commit_idle" doc:"Commit a document after this long without writes; 0 turns it off."`
	SSHKey          string   `yaml:"ssh_key" doc:"Path to an SSH private key for pushing."`
	// Credentials are the accounts a script's Git remotes push with.
	Credentials []GitCredential `yaml:"credentials" doc:"Accounts for pushing each script's remotes (GitHub, GitLab, Bitbucket), by name. YAML only; put tokens in with ${VAR}."`
	Token       string          `yaml:"token" secret:"true" doc:"HTTPS token for pushing (SCREENJSON_GIT_TOKEN)."`
}

// GitCredential is an account a Git remote pushes with.
type GitCredential struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"` // github, gitlab, bitbucket or generic
	Username string `yaml:"username"`
	Token    string `yaml:"token" secret:"true"`
}

// Blob is blob storage for imports and exports (5.7).
type Blob struct {
	Driver    string `yaml:"driver" doc:"s3, minio, azure or fs."`
	Endpoint  string `yaml:"endpoint" doc:"Endpoint URL, for minio or another S3-compatible store."`
	Region    string `yaml:"region" doc:"S3 region."`
	Bucket    string `yaml:"bucket" doc:"Default bucket, used when a target URL names none."`
	AccessKey string `yaml:"access_key" secret:"true" doc:"S3 or MinIO access key; the Azure account name for azure."`
	SecretKey string `yaml:"secret_key" secret:"true" doc:"S3 or MinIO secret key; the Azure account key for azure."`
	Root      string `yaml:"root" doc:"Base directory for the fs driver."`
}

// Greenlight is the sibling service that runs CLI-level work (exports,
// conversions) for the server as queued jobs. See internal/greenlight.
type Greenlight struct {
	Servers       []string     `yaml:"servers" live:"true" doc:"Up to 5 Greenlight servers as host:port. Jobs are spread over the ones that are up, fewest unfinished jobs first; a server that fails to take one is skipped. Comma-separated in the environment."`
	TLS           bool         `yaml:"tls" live:"true" doc:"Talk to the Greenlight servers over HTTPS."`
	APIKey        string       `yaml:"api_key" secret:"true" live:"true" doc:"Greenlight's SHARED_API_KEY, sent as a bearer token."`
	CheckInterval Duration     `yaml:"check_interval" doc:"How often each Greenlight server is pinged."`
	Handover      string       `yaml:"handover" live:"true" doc:"How a job's input reaches Greenlight: storage (written to its input bucket) or upload (its POST /upload)."`
	S3            GreenlightS3 `yaml:"s3"`
}

// GreenlightS3 is the object store Greenlight reads job input from, for the
// storage handover.
type GreenlightS3 struct {
	Endpoint  string `yaml:"endpoint" live:"true" doc:"S3 API URL for MinIO or another S3-compatible store; empty for AWS S3."`
	Region    string `yaml:"region" live:"true" doc:"S3 region."`
	AccessKey string `yaml:"access_key" secret:"true" live:"true" doc:"Access key."`
	SecretKey string `yaml:"secret_key" secret:"true" live:"true" doc:"Secret key."`
	Bucket    string `yaml:"bucket" live:"true" doc:"Greenlight's S3_INPUT_BUCKET; read from Greenlight's /setup when empty."`
	Prefix    string `yaml:"prefix" live:"true" doc:"Greenlight's S3_INPUT_PREFIX; read from Greenlight's /setup with the bucket."`
}

// Size is a byte count written like 32MiB or 1GiB.
type Size int64

var sizeUnits = map[string]int64{
	"": 1, "b": 1, "kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30,
}

var sizePattern = regexp.MustCompile(`^\s*(\d+)\s*([A-Za-z]*)\s*$`)

// ParseSize reads a size.
func ParseSize(s string) (Size, error) {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a size like 32MiB", s)
	}
	mult, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit (use B, KiB, MiB, GiB, KB, MB or GB)", s)
	}
	var n int64
	if _, err := fmt.Sscan(m[1], &n); err != nil {
		return 0, err
	}
	return Size(n * mult), nil
}

// String writes the size in the largest binary unit that divides it.
func (s Size) String() string {
	for _, u := range []struct {
		name string
		n    int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if s != 0 && int64(s)%u.n == 0 {
			return fmt.Sprintf("%d%s", int64(s)/u.n, u.name)
		}
	}
	return fmt.Sprintf("%dB", int64(s))
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (s Size) MarshalYAML() (any, error) { return s.String(), nil }

// Duration is a time span written like 300s or 10m; a bare number is seconds.
type Duration time.Duration

// ParseDuration reads a duration.
func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if regexp.MustCompile(`^\d+$`).MatchString(s) {
		s += "s"
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 300s", s)
	}
	return Duration(d), nil
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// LayoutSetting is storage.layout: a preset name or a full layout.
type LayoutSetting struct {
	// Preset is set when the layout was given by name.
	Preset string
	// Layout is the resolved layout.
	Layout layout.Layout
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *LayoutSetting) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return l.Set(n.Value)
	}
	var lay layout.Layout
	if err := n.Decode(&lay); err != nil {
		return fmt.Errorf("storage.layout: %w", err)
	}
	l.Preset, l.Layout = "", lay
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (l LayoutSetting) MarshalYAML() (any, error) {
	if l.Preset != "" {
		return l.Preset, nil
	}
	return l.Layout, nil
}

// Set reads a preset name or a JSON layout (R-CFG-04).
func (l *LayoutSetting) Set(s string) error {
	lay, err := layout.Parse(s)
	if err != nil {
		return err
	}
	l.Layout = lay
	l.Preset = ""
	if !strings.HasPrefix(strings.TrimSpace(s), "{") {
		l.Preset = strings.TrimSpace(s)
	}
	return nil
}

// Default returns the built-in defaults (SPEC.md 12.2, with the memory driver
// so that starting with nothing configured works).
func Default() *Config {
	c := &Config{Mode: "full", DataDir: "/data"}
	c.Log.Level = "info"
	c.HTTP = HTTP{Addr: "127.0.0.1:8080", MaxBody: 32 << 20, MCP: true}
	c.Storage.Driver = "memory"
	c.Storage.EnvelopeField = "sj"
	c.Storage.Layout = LayoutSetting{Preset: layout.PresetDefault}
	c.Storage.Layout.Layout, _ = layout.Preset(layout.PresetDefault)
	c.Storage.Elastic.Refresh = "wait_for"
	c.Storage.Chroma.Tenant = "default_tenant"
	c.Storage.Chroma.Database = "default_database"
	c.Storage.Weaviate.ClassPrefix = "Sj"
	c.Cache = Cache{MaxDocuments: 256, MaxBytes: 1 << 30}
	c.Queue.Length = 1000
	c.Checkouts = Checkouts{DefaultTTL: Duration(300 * time.Second), MaxTTL: Duration(3600 * time.Second)}
	c.Numbering.Default = "none"
	c.Events.Backend = "embedded"
	c.Events.HistorySize = 1000
	c.Events.HistoryTTL = Duration(10 * time.Minute)
	c.Git.Mode = "repo-per-doc"
	c.Git.Branch = "main"
	c.Git.Push = true
	c.Git.Author.Name = "screenjson-server"
	c.Git.Author.Email = "screenjson@localhost"
	c.Blob.Driver = "fs"
	c.Blob.Region = "us-west-2"
	c.Greenlight.CheckInterval = Duration(15 * time.Second)
	c.Greenlight.Handover = "storage"
	c.Greenlight.S3.Region = "us-east-1"
	return c
}

// TransactionsOn reports whether multi-record writes use transactions: the
// setting if given, else true on mongo and postgres (R-ARCH-06).
func (c *Config) TransactionsOn() bool {
	if c.Storage.Transactions != nil {
		return *c.Storage.Transactions
	}
	return c.Storage.Driver == "mongo" || c.Storage.Driver == "postgres"
}

// GitWorkdir is git.workdir, or data_dir/git.
func (c *Config) GitWorkdir() string {
	if c.Git.Workdir != "" {
		return c.Git.Workdir
	}
	return strings.TrimRight(c.DataDir, "/") + "/git"
}

// placeholder matches ${VAR} and ${VAR:-default}.
var placeholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Expand fills placeholders from the environment (R-CFG-02). An unset
// variable without a default becomes empty.
func Expand(s string, getenv func(string) string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		sub := placeholder.FindStringSubmatch(m)
		if v := getenv(sub[1]); v != "" {
			return v
		}
		return sub[2]
	})
}

// Options says where Load reads from.
type Options struct {
	// File is the YAML file; empty means SCREENJSON_CONFIG, or none.
	File string
	// Flags are command-line settings as "path=value", highest precedence.
	Flags []string
	// Getenv reads the environment; nil means os.Getenv.
	Getenv func(string) string
	// Environ lists the environment, for aliases; nil means os.Environ.
	Environ func() []string
}

// Load builds the configuration and validates it.
func Load(o Options) (*Config, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Default()
	c.sources = map[string]Source{}

	file := o.File
	if file == "" {
		file = getenv("SCREENJSON_CONFIG")
	}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", file, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(Expand(string(raw), getenv)))
		dec.KnownFields(true)
		if err := dec.Decode(c); err != nil {
			return nil, fmt.Errorf("config: %s: %w", file, err)
		}
		c.file = file
		for _, path := range filePaths(raw) {
			c.sources[path] = SourceFile
		}
	}

	if err := applyAliases(c, getenv); err != nil {
		return nil, err
	}
	for _, f := range Fields() {
		if v := getenv(f.Env); v != "" {
			if err := f.set(c, v); err != nil {
				return nil, fmt.Errorf("config: %s: %w", f.Env, err)
			}
			c.sources[f.Path] = SourceEnv
		}
	}
	for _, kv := range o.Flags {
		path, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("config: flag %q is not path=value", kv)
		}
		f, ok := FieldByPath(path)
		if !ok {
			return nil, fmt.Errorf("config: no setting %q", path)
		}
		if err := f.set(c, v); err != nil {
			return nil, fmt.Errorf("config: --set %s: %w", path, err)
		}
		c.sources[f.Path] = SourceFlag
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Aliases are the CLI's environment names accepted with the same meaning
// (12.1), mapped to the setting they fill. They are read before the server's
// own names, which therefore win when both are set.
var Aliases = map[string]string{
	"SCREENJSON_DB_TYPE":        "storage.driver",
	"SCREENJSON_BLOB_TYPE":      "blob.driver",
	"SCREENJSON_BLOB_BUCKET":    "blob.bucket",
	"SCREENJSON_AWS_ACCESS_KEY": "blob.access_key",
	"SCREENJSON_AWS_SECRET_KEY": "blob.secret_key",
	"SCREENJSON_AWS_REGION":     "blob.region",
}

// applyAliases fills settings from the CLI's names, and builds storage.url
// from SCREENJSON_DB_HOST, _PORT, _USER and _PASS when no URL is set.
func applyAliases(c *Config, getenv func(string) string) error {
	for env, path := range Aliases {
		v := getenv(env)
		if v == "" {
			continue
		}
		f, _ := FieldByPath(path)
		c.sources[f.Path] = SourceEnv
		if err := f.set(c, v); err != nil {
			return fmt.Errorf("config: %s: %w", env, err)
		}
	}
	host := getenv("SCREENJSON_DB_HOST")
	if host == "" || c.Storage.URL != "" || getenv("SCREENJSON_STORAGE_URL") != "" {
		return nil
	}
	scheme := map[string]string{"mongo": "mongodb", "postgres": "postgres"}[c.Storage.Driver]
	if scheme == "" {
		scheme = "http"
	}
	userinfo := ""
	if u := getenv("SCREENJSON_DB_USER"); u != "" {
		userinfo = u
		if p := getenv("SCREENJSON_DB_PASS"); p != "" {
			userinfo += ":" + p
		}
		userinfo += "@"
	}
	hostport := host
	if p := getenv("SCREENJSON_DB_PORT"); p != "" {
		hostport += ":" + p
	}
	c.Storage.URL = scheme + "://" + userinfo + hostport
	c.sources["storage.url"] = SourceEnv
	return nil
}

// Choices are the values allowed for settings that take one of a list.
var Choices = map[string][]string{
	"mode":                    {"full", "headless"},
	"log.level":               {"debug", "info", "warn", "error"},
	"storage.driver":          {"memory", "mongo", "postgres", "elastic", "chroma", "weaviate", "pinecone"},
	"numbering.default":       {"none", "auto", "frozen"},
	"events.backend":          {"embedded", "centrifugo"},
	"git.mode":                {"repo-per-doc", "mono"},
	"blob.driver":             {"s3", "minio", "azure", "fs"},
	"greenlight.handover":     {"storage", "upload"},
	"storage.elastic.refresh": {"wait_for", "true", "false"},
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Validate checks the whole configuration and reports every problem at once.
func (c *Config) Validate() error {
	var problems []string
	oneOf := func(name, v string, allowed ...string) {
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s is %q; want one of %s", name, v, strings.Join(allowed, ", ")))
	}
	for _, path := range sortedKeys(Choices) {
		f, _ := FieldByPath(path)
		oneOf(path, f.value(c), Choices[path]...)
	}

	if c.DataDir == "" {
		problems = append(problems, "data_dir is empty")
	}
	if c.Storage.Driver != "memory" && c.Storage.URL == "" {
		problems = append(problems, fmt.Sprintf("storage.url is required for the %s driver", c.Storage.Driver))
	}
	if c.Storage.Driver == "pinecone" && c.Storage.Pinecone.APIKey == "" {
		problems = append(problems, "storage.pinecone.api_key is required for pinecone")
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`).MatchString(c.Storage.EnvelopeField) {
		problems = append(problems, fmt.Sprintf("storage.envelope_field %q is not a plain field name", c.Storage.EnvelopeField))
	}
	if c.Storage.Transactions != nil && *c.Storage.Transactions &&
		c.Storage.Driver != "mongo" && c.Storage.Driver != "postgres" {
		problems = append(problems, fmt.Sprintf("storage.transactions: the %s driver has no transactions", c.Storage.Driver))
	}
	if err := c.Storage.Layout.Layout.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if (c.Storage.Driver == "chroma" || c.Storage.Driver == "weaviate" || c.Storage.Driver == "pinecone") && !c.Storage.AllowLargeRecords &&
		!c.Storage.Layout.Layout.Splits("scene") {
		problems = append(problems, fmt.Sprintf(
			"storage.layout: the %s driver would hold each whole screenplay in one record, which gives one useless vector per script and very large records; "+
				"split scenes out, or set storage.allow_large_records: true (R-DRV-07)", c.Storage.Driver))
	}
	if c.HTTP.MaxBody <= 0 {
		problems = append(problems, "http.max_body must be positive")
	}
	if c.Cache.MaxDocuments < 1 || c.Cache.MaxBytes < 1 {
		problems = append(problems, "cache.max_documents and cache.max_bytes must be positive")
	}
	if c.Queue.Length < 1 {
		problems = append(problems, "queue.length must be positive")
	}
	if c.Checkouts.DefaultTTL <= 0 || c.Checkouts.MaxTTL < c.Checkouts.DefaultTTL {
		problems = append(problems, "checkouts: default_ttl must be positive and no more than max_ttl")
	}
	if c.Events.HistorySize < 0 || c.Events.HistoryTTL < 0 {
		problems = append(problems, "events.history_size and history_ttl can't be negative")
	}
	if c.Events.Backend == "centrifugo" && (c.Events.Centrifugo.APIURL == "" || c.Events.Centrifugo.ClientURL == "") {
		problems = append(problems, "events.centrifugo.api_url and client_url are required with the centrifugo backend")
	}
	if c.Git.Enabled && c.Git.Push && c.Git.Remote == "" {
		problems = append(problems, "git.remote is required when git.push is on")
	}
	credNames := map[string]bool{}
	for i, cr := range c.Git.Credentials {
		if cr.Name == "" || cr.Token == "" {
			problems = append(problems, fmt.Sprintf("git.credentials[%d] needs a name and a token", i))
		}
		if credNames[cr.Name] {
			problems = append(problems, fmt.Sprintf("git.credentials: name %q used twice", cr.Name))
		}
		credNames[cr.Name] = true
		switch cr.Provider {
		case "", "github", "gitlab", "bitbucket", "generic":
		default:
			problems = append(problems, fmt.Sprintf("git.credentials[%d]: provider must be github, gitlab, bitbucket or generic", i))
		}
	}
	if len(c.Greenlight.Servers) > 5 {
		problems = append(problems, "greenlight.servers: at most 5")
	}
	for _, sv := range c.Greenlight.Servers {
		host, port, err := net.SplitHostPort(strings.TrimSpace(sv))
		if n, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
			problems = append(problems, fmt.Sprintf("greenlight.servers: %q is not host:port", sv))
		}
	}
	if c.Greenlight.CheckInterval <= 0 {
		problems = append(problems, "greenlight.check_interval must be positive")
	}
	names := map[string]bool{}
	for i, w := range c.Webhooks {
		if w.Name == "" || w.URL == "" || len(w.On) == 0 {
			problems = append(problems, fmt.Sprintf("webhooks[%d] needs a name, a url and at least one event in on", i))
		}
		if names[w.Name] {
			problems = append(problems, fmt.Sprintf("webhooks: name %q used twice", w.Name))
		}
		names[w.Name] = true
		for _, ev := range w.On {
			switch ev {
			case "text.changed", "node.inserted", "node.removed", "node.moved", "node.updated",
				"document.created", "document.deleted":
			default:
				problems = append(problems, fmt.Sprintf("webhooks[%d]: unknown event %q", i, ev))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}
