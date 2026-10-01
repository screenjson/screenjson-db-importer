// Package blob reads and writes files in object storage for imports and
// exports (SPEC.md 5.7, R-BLOB-01). URLs name the place:
//
//	s3://bucket/prefix/file.jscn      S3, or MinIO when blob.driver is minio
//	azure://container/path/file.pdf   Azure Blob Storage
//	file:///data/exports/file.pdf     the local filesystem
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/screenjson/screenjson-db-importer/internal/config"
)

// Store is one kind of object storage.
type Store interface {
	Put(ctx context.Context, bucket, key string, data []byte, contentType string) error
	Get(ctx context.Context, bucket, key string) ([]byte, error)
	// List returns the keys under a prefix, sorted.
	List(ctx context.Context, bucket, prefix string) ([]string, error)
}

// Blobs routes URLs to the configured stores.
type Blobs struct {
	s3    Store
	azure Store
	fs    Store
	cfg   config.Blob
}

// New builds the stores the configuration allows. S3 and Azure clients are
// made on first use, so a server that never exports needs no credentials.
func New(c config.Blob) *Blobs {
	return &Blobs{cfg: c, fs: &fsStore{root: c.Root}}
}

// Location is a parsed blob URL.
type Location struct {
	Scheme, Bucket, Key string
}

// Parse splits a blob URL.
func Parse(raw string) (Location, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Location{}, fmt.Errorf("blob: %q is not a URL", raw)
	}
	switch u.Scheme {
	case "s3", "azure":
		if u.Host == "" {
			return Location{}, fmt.Errorf("blob: %q names no bucket or container", raw)
		}
		return Location{Scheme: u.Scheme, Bucket: u.Host, Key: strings.TrimPrefix(u.Path, "/")}, nil
	case "file":
		return Location{Scheme: "file", Key: u.Path}, nil
	}
	return Location{}, fmt.Errorf("blob: %q: the scheme must be s3, azure or file", raw)
}

func (b *Blobs) store(scheme string) (Store, error) {
	switch scheme {
	case "file":
		return b.fs, nil
	case "s3":
		if b.s3 == nil {
			s, err := newS3(b.cfg)
			if err != nil {
				return nil, err
			}
			b.s3 = s
		}
		return b.s3, nil
	case "azure":
		if b.azure == nil {
			s, err := newAzure(b.cfg)
			if err != nil {
				return nil, err
			}
			b.azure = s
		}
		return b.azure, nil
	}
	return nil, fmt.Errorf("blob: unknown scheme %q", scheme)
}

// Put writes data at a URL.
func (b *Blobs) Put(ctx context.Context, raw string, data []byte, contentType string) error {
	loc, err := Parse(raw)
	if err != nil {
		return err
	}
	s, err := b.store(loc.Scheme)
	if err != nil {
		return err
	}
	return s.Put(ctx, loc.Bucket, loc.Key, data, contentType)
}

// Get reads a URL.
func (b *Blobs) Get(ctx context.Context, raw string) ([]byte, error) {
	loc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	s, err := b.store(loc.Scheme)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, loc.Bucket, loc.Key)
}

// List returns the URLs under a prefix URL.
func (b *Blobs) List(ctx context.Context, raw string) ([]string, error) {
	loc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	s, err := b.store(loc.Scheme)
	if err != nil {
		return nil, err
	}
	keys, err := s.List(ctx, loc.Bucket, loc.Key)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		switch loc.Scheme {
		case "file":
			out[i] = "file://" + k
		default:
			out[i] = loc.Scheme + "://" + loc.Bucket + "/" + k
		}
	}
	return out, nil
}

// ---- S3 and MinIO ----

type s3Store struct{ c *minio.Client }

// newS3 connects to S3, or to MinIO when blob.driver is minio. With a static
// key and secret those are used; otherwise the standard AWS chain applies —
// environment, shared config, then instance or task role — so one image works
// on a laptop and in a cluster (the pattern Greenlight uses).
func newS3(c config.Blob) (Store, error) {
	endpoint, secure := "s3.amazonaws.com", true
	if c.Region != "" && c.Driver != "minio" {
		endpoint = "s3." + c.Region + ".amazonaws.com"
	}
	if c.Endpoint != "" {
		u := c.Endpoint
		secure = !strings.HasPrefix(u, "http://")
		u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
		endpoint = strings.TrimRight(u, "/")
	}
	var creds *credentials.Credentials
	if c.AccessKey != "" && c.SecretKey != "" {
		creds = credentials.NewStaticV4(c.AccessKey, c.SecretKey, "")
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{}, &credentials.FileAWSCredentials{}, &credentials.IAM{},
		})
	}
	opts := &minio.Options{Creds: creds, Secure: secure, Region: c.Region}
	if c.Driver == "minio" {
		opts.BucketLookup = minio.BucketLookupPath
	}
	cl, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 client: %w", err)
	}
	return &s3Store{c: cl}, nil
}

func (s *s3Store) Put(ctx context.Context, bucket, key string, data []byte, ct string) error {
	_, err := s.c.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: ct})
	if err != nil {
		return fmt.Errorf("blob: put s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

func (s *s3Store) Get(ctx context.Context, bucket, key string) ([]byte, error) {
	obj, err := s.c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blob: get s3://%s/%s: %w", bucket, key, err)
	}
	defer obj.Close()
	b, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("blob: get s3://%s/%s: %w", bucket, key, err)
	}
	return b, nil
}

func (s *s3Store) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	var out []string
	for o := range s.c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, fmt.Errorf("blob: list s3://%s/%s: %w", bucket, prefix, o.Err)
		}
		out = append(out, o.Key)
	}
	sort.Strings(out)
	return out, nil
}

// ---- Azure ----

type azureStore struct{ c *azblob.Client }

// newAzure connects with a shared key: blob.access_key is the account name
// and blob.secret_key the account key. blob.endpoint overrides the service
// URL (for Azurite).
func newAzure(c config.Blob) (Store, error) {
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("blob: azure needs blob.access_key (account) and blob.secret_key (key)")
	}
	cred, err := azblob.NewSharedKeyCredential(c.AccessKey, c.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("blob: azure credential: %w", err)
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "https://" + c.AccessKey + ".blob.core.windows.net/"
	}
	cl, err := azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("blob: azure client: %w", err)
	}
	return &azureStore{c: cl}, nil
}

func (s *azureStore) Put(ctx context.Context, container, key string, data []byte, _ string) error {
	if _, err := s.c.UploadBuffer(ctx, container, key, data, nil); err != nil {
		return fmt.Errorf("blob: put azure://%s/%s: %w", container, key, err)
	}
	return nil
}

func (s *azureStore) Get(ctx context.Context, container, key string) ([]byte, error) {
	res, err := s.c.DownloadStream(ctx, container, key, nil)
	if err != nil {
		return nil, fmt.Errorf("blob: get azure://%s/%s: %w", container, key, err)
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

func (s *azureStore) List(ctx context.Context, container, prefix string) ([]string, error) {
	var out []string
	pager := s.c.NewListBlobsFlatPager(container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blob: list azure://%s/%s: %w", container, prefix, err)
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name != nil {
				out = append(out, *item.Name)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- Local filesystem ----

// fsStore writes under a root directory when blob.root is set, and anywhere
// otherwise.
type fsStore struct{ root string }

func (s *fsStore) path(key string) (string, error) {
	p := filepath.Clean("/" + key)
	if s.root == "" {
		return p, nil
	}
	root := filepath.Clean(s.root)
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("blob: %s is outside blob.root %s", p, root)
	}
	return p, nil
}

func (s *fsStore) Put(_ context.Context, _, key string, data []byte, _ string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("blob: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("blob: %w", err)
	}
	return os.Rename(tmp, p)
}

func (s *fsStore) Get(_ context.Context, _, key string) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (s *fsStore) List(_ context.Context, _, prefix string) ([]string, error) {
	p, err := s.path(prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(p, func(q string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, q)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("blob: list %s: %w", p, err)
	}
	sort.Strings(out)
	return out, nil
}

// Name fills {doc} and {lang} in a target template.
func Name(target, doc, lang string) string {
	return strings.NewReplacer("{doc}", doc, "{lang}", lang).Replace(target)
}

// Ext returns a key's extension, lowercased.
func Ext(key string) string { return strings.ToLower(path.Ext(key)) }
