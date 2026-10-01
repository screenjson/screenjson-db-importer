# screenjson-db-importer

`screenjson-db-importer` imports ScreenJSON `.json` files into a database. First create the files with [`screenjson-cli`](https://github.com/screenjson/screenjson-cli), the separate open-source converter for PDF, Final Draft (FDX), Fountain, and other screenplay formats. This tool handles the database import step; it does not convert source screenplays.

The importer reads the complete ScreenJSON document and validates it against the ScreenJSON schema before writing. Its YAML configuration selects the database and blob storage services, connection details, and document layout. The YAML format and stored record format are compatible with the other ScreenJSON tools, while this repository builds and runs as an independent CLI.

## Install

Build from source with Go:

```sh
go build -o screenjson-db-importer .
```

After publishing the image to GHCR, pull and run it:

```sh
docker pull ghcr.io/screenjson/screenjson-db-importer:latest
docker run --rm \
  -v "$PWD:/data" \
  -v "$PWD/importer.yaml:/config/importer.yaml:ro" \
  ghcr.io/screenjson/screenjson-db-importer:latest \
  --config /config/importer.yaml /data/converted/
```

## Configure

Create an `importer.yaml` file. For example, this uses PostgreSQL for records and local files for source documents:

```yaml
data_dir: /data/.screenjson-importer

storage:
  driver: postgres
  url: postgres://screenjson:secret@database:5432/screenplays?sslmode=disable
  database: screenplays
  layout: scenes
  transactions: true

blob:
  driver: fs
  root: /data
```

For MongoDB, Elasticsearch, Chroma, Weaviate, or Pinecone, set `storage.driver` and its connection settings below. Pinecone requires an existing dense serverless index; `storage.url` is its index host and `storage.pinecone.api_key` is the API key. Its optional `namespace` is a prefix, with a separate namespace used for each collection. Set `storage.allow_large_records: true` only when intentionally using a layout that stores a whole screenplay as one vector record.

For S3-compatible storage such as AWS S3, MinIO, or DigitalOcean Spaces, configure `blob.driver`, `blob.endpoint`, `blob.region`, `blob.bucket`, `blob.access_key`, and `blob.secret_key`. For Azure, use `blob.driver: azure` and the account name and key in `blob.access_key` and `blob.secret_key`. Local files use `blob.driver: fs` and `blob.root`.

Values can also come from environment variables (for example, `SCREENJSON_STORAGE_URL` and `SCREENJSON_STORAGE_PINECONE_API_KEY`) or command-line overrides with `--set path=value`. In YAML, `${VAR}` and `${VAR:-default}` expand from the environment. See the full setting reference below.

## Import

The input can be one JSON file, a directory, a glob, or a configured `file://`, `s3://`, or `azure://` URI:

```sh
screenjson-db-importer --config importer.yaml --workers 8 \
  --manifest import-state.jsonl --retry-failed ./converted/
```

Preflight checks every source for readability, valid JSON, and ScreenJSON schema validity before any writes. Processing rereads each source and verifies its SHA-256 hash. The JSONL manifest records per-file results and lets you resume successful imports. Matching IDs and content are treated as already imported; the same ID with different content is an error. A run exits nonzero when any file fails. Concurrent imports are refused while another ScreenJSON writer holds the database state lock.

## Configuration reference

Configuration values are resolved in this order: `--set path=value`, environment variables, this YAML file (`--config` or `SCREENJSON_CONFIG`), then defaults. YAML values support `${VAR}` and `${VAR:-default}` environment expansion. Database and blob settings are described here; unlisted server, UI, and job settings are not needed by this importer.

| Setting | Environment variable | Default | Meaning |
|---|---|---|---|
| `data_dir` | `SCREENJSON_DATA_DIR` | `/data` | Local directory for importer state and the concurrency lock. |
| `storage.driver` | `SCREENJSON_STORAGE_DRIVER` | `memory` | `memory`, `mongo`, `postgres`, `elastic`, `chroma`, `weaviate`, or `pinecone`. |
| `storage.url` | `SCREENJSON_STORAGE_URL` | empty | Database URL; required except for `memory`. |
| `storage.database` | `SCREENJSON_STORAGE_DATABASE` | empty | Database name where supported. |
| `storage.transactions` | `SCREENJSON_STORAGE_TRANSACTIONS` | driver default | Multi-record transactions; available for MongoDB and PostgreSQL. |
| `storage.envelope_field` | `SCREENJSON_STORAGE_ENVELOPE_FIELD` | `sj` | Envelope field for MongoDB and Elasticsearch records. |
| `storage.allow_large_records` | `SCREENJSON_STORAGE_ALLOW_LARGE_RECORDS` | `false` | Allow Chroma, Weaviate, or Pinecone to keep a whole screenplay in one record. |
| `storage.layout` | `SCREENJSON_STORAGE_LAYOUT` | `elements` | `whole`, `scenes`, `elements`, or a YAML map that defines the record layout. |
| `storage.elastic.refresh` | `SCREENJSON_STORAGE_ELASTIC_REFRESH` | `wait_for` | Elasticsearch refresh mode: `wait_for`, `true`, or `false`. |
| `storage.chroma.tenant` | `SCREENJSON_STORAGE_CHROMA_TENANT` | `default_tenant` | Chroma tenant. |
| `storage.chroma.database` | `SCREENJSON_STORAGE_CHROMA_DATABASE` | `default_database` | Chroma database. |
| `storage.weaviate.class_prefix` | `SCREENJSON_STORAGE_WEAVIATE_CLASS_PREFIX` | `Sj` | Prefix for Weaviate collection names. |
| `storage.pinecone.api_key` | `SCREENJSON_STORAGE_PINECONE_API_KEY` | empty | Pinecone API key. |
| `storage.pinecone.namespace` | `SCREENJSON_STORAGE_PINECONE_NAMESPACE` | empty | Prefix for the collection namespaces. |
| `blob.driver` | `SCREENJSON_BLOB_DRIVER` | `fs` | `fs`, `s3`, `minio`, or `azure`. |
| `blob.endpoint` | `SCREENJSON_BLOB_ENDPOINT` | empty | S3-compatible endpoint; empty for AWS S3. |
| `blob.region` | `SCREENJSON_BLOB_REGION` | `us-west-2` | S3 region. |
| `blob.bucket` | `SCREENJSON_BLOB_BUCKET` | empty | Default object-store bucket. |
| `blob.access_key` | `SCREENJSON_BLOB_ACCESS_KEY` | empty | S3/MinIO access key or Azure account name. |
| `blob.secret_key` | `SCREENJSON_BLOB_SECRET_KEY` | empty | S3/MinIO secret key or Azure account key. |
| `blob.root` | `SCREENJSON_BLOB_ROOT` | empty | Base directory for the filesystem blob driver. |

`storage.layout` maps ScreenJSON document levels to database collections and is how you shape storage for your application. The importer does not require your application to adopt a particular database schema beyond the ScreenJSON record envelope; collection names and layout can be configured in YAML. Records that do not contain embeddings remain valid records. Vector databases store a zero vector with metadata marking it as non-native.
