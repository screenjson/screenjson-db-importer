# `screenjson-db-import` — implementation brief

## Goal and pipeline boundary

The automation pipeline has separate stages:

1. `screenjson-cli` converts FDX, Fade In, Fountain, PDF, and other supported
   source formats into ScreenJSON `.json` files.
2. `screenjson-db-import` reads those `.json` files from local disk, Azure
   Blob, AWS S3, MinIO, or DigitalOcean Spaces and writes them into the
   configured database.

This tool accepts ScreenJSON JSON only. It owns source enumeration, JSON
validation, database connections/writes, batching, retries, progress,
resumability, and reporting. It does not call Greenlight or repeat conversion.
At 10,000+ documents it must stream work with bounded memory and configurable
concurrency.

## Reuse the server's persistence engine

“Own database interactions” means the importer performs the load directly
against the selected database, without requiring the server process or HTTP
upload API. It must still use the **server's** configured store driver and
document creation/layout logic. Do not use the older `screenjson-cli`
database adapters as the writer: they store a different document model and
shape (for example, the CLI Mongo adapter writes one `model.Document` into a
`documents` collection, while the server stores layout-dependent records with
envelopes, ordering and revisions).

The server has most of this path. Because the CLI is independently released
and open source while the server is a private product, the CLI keeps local
copies of the required configuration, schema, document manager, layout, state,
blob and store-driver code. It has no dependency on the server module. Changes
to duplicated code and persisted record semantics must be synchronized.

- `internal/admin.Import` calls `app.New` and `docs.Manager.Create` for each
  JSON document. This applies server schema checks, IDs, layout, derived
  records and storage semantics without HTTP.
- `docs.Manager.Create` validates each complete imported document against the
  server's compiled schema before persisting it. Reuse this validation path
  in the importer's copied implementation; do not create a competing JSON schema.
- `internal/store/drivers.Open` opens the configured server database driver.
- `internal/blob` supplies filesystem, Azure, S3 and MinIO object access.
- `cmd/screenjson-server`'s `import` command has local traversal/glob,
  worker-pool, output and config-loading patterns. Its online branch posts to
  `/documents`; its offline branch currently reads all files into memory.

Implement `screenjson-db-importer` as a separate executable in this
repository. It has no source or runtime dependency on screenjson-server. Keep
its copied manager, layout, validation, and drivers compatible with the
server. Direct DB mode must observe the server lease/state lock and refuse to
run while the server is active. Document the stop-server requirement clearly.

## Config contract: use the server YAML

Load the same server config file and environment overrides; do not create a
second set of backend-specific flags or a new incompatible config format.
Database selection and physical record shape are determined by
`storage.driver`, `storage.layout`, and related server settings. Source object
access uses `blob:` settings. Representative shape:

```yaml
storage:
  driver: mongo # memory | mongo | postgres | elastic | chroma | weaviate | pinecone
  url: ${SCREENJSON_STORAGE_URL}
  database: screenjson
  layout: elements
  # Driver settings: elastic.refresh; chroma.tenant/database;
  # weaviate.class_prefix; pinecone.api_key/namespace; envelope_field, etc.
blob:
  driver: s3 # fs | azure | s3 | minio
  endpoint: ${SCREENJSON_BLOB_ENDPOINT}
  region: ${SCREENJSON_BLOB_REGION}
  bucket: ${SCREENJSON_BLOB_BUCKET}
  access_key: ${SCREENJSON_BLOB_ACCESS_KEY}
  secret_key: ${SCREENJSON_BLOB_SECRET_KEY}
  root: ${SCREENJSON_BLOB_ROOT}
```

Use `blob.driver: fs` plus `root` for local files, `azure` for Azure Blob,
`s3` for AWS S3, `minio` plus endpoint for MinIO, and the S3-compatible
`endpoint`/region/credentials for DigitalOcean Spaces. Keep the server's URI
and config conventions (including how bucket/container and prefixes are
resolved) rather than inventing provider-specific URL rules.

## Database support matrix and gaps

Both products support `mongo`, `postgres`, `elastic`, `chroma`, `weaviate`,
`pinecone`, and `memory`, with aligned YAML settings and persisted record
formats. Although Postgres is SQL, keep it in the compatibility matrix.

The `screenjson-cli` Pinecone adapter is only a stub returning “not yet
implemented”; it cannot be reused as a solution. Pinecone support is
implemented independently in both products: an existing dense serverless
index, one namespace per collection, and the same metadata and vector
semantics. Do not let the copies drift.

## Import behavior

- Input: one `.json`, directory, glob, or configured storage URI/prefix.
  Enumerate deterministically, preserve source identity, and validate that
  files are JSON ScreenJSON before writing.
- Validation: parse every candidate as JSON and validate its ScreenJSON
  structure/content using the importer's synchronized schema and document checks before it
  is counted as importable. Prefer a preflight phase that reports all bad
  files before writes begin; `docs.Manager.Create` remains the final
  authoritative validation at write time. Report filename, JSON path/pointer,
  and validation message, then skip invalid input. Keep the copied validator
  aligned with the server's validator.
- Throughput: configurable worker count; bounded queue and per-file memory;
  do not materialize the full corpus in a map. Use the importer's duplicated
  streaming import flow, aligned with the server's document create path.
- Safety: stop on config/connection/state-lock errors; report per-file schema
  or storage failures and continue where safe. Use server document IDs and
  existing duplicate-ID semantics; define behavior for a repeated source,
  duplicate ID with same content, and duplicate ID with different content.
- Restart: durable manifest keyed by source path or bucket/key, size, and
  checksum where feasible; record document ID, status, and error. On resume,
  skip confirmed successes and retry only uncommitted/failed work according
  to an explicit retry option.
- Output: human-readable progress and summary, optional JSONL/manifest output,
  and nonzero exit status if any input fails. Never log credentials.

## First-release exclusions

- Format conversion and non-JSON parsing; these belong to `screenjson-cli`.
- HTTP upload to a running server; direct configured DB import is the purpose
  of this command, and offline locking rules apply.
- Direct writes that bypass server `docs.Manager` or server storage drivers.
- Pinecone index creation and control-plane management; configure an existing
  dense serverless index in YAML.

## Completion criteria

Import validated ScreenJSON JSON from local disk, Azure Blob, AWS S3, MinIO, and
DigitalOcean Spaces into each implemented server database driver using the
server YAML config. Confirm bounded resource use, server-compatible records,
lock protection while the server is running, resumability, accurate per-file
errors, and correct handling of repeated document IDs.
