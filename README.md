# screenjson-db-importer

Standalone, open source CLI for importing converted ScreenJSON `.json` files
directly into a configured database. Its Go module has no dependency on the
private `screenjson-server` module. It contains its own copy of the required
configuration, schema validation, document manager, layouts, blob clients,
state protection, and database drivers. These copies intentionally keep the
same YAML contract and persisted record format as the server, while allowing
the CLI to be built, released, and used independently.

Build from this repository:

```sh
go build -o screenjson-db-importer .
```

Stop `screenjson-server` before direct import. The importer uses the same state
lock and lease checks and refuses to run while the server is active.

```sh
./screenjson-db-importer --config server.yaml --workers 8 \
  --manifest import-state.jsonl --retry-failed ./converted/
```

The source may be one JSON file, a directory, glob, or configured `file://`,
`s3://`, or `azure://` URI. DigitalOcean Spaces uses the S3-compatible blob
configuration. Preflight validates every source with the copied ScreenJSON
validator and document manager before writes; processing rereads each source
and checks its SHA-256. The synced JSONL manifest resumes successful sources
and records per-file errors. Duplicate IDs use server behavior: identical
content succeeds idempotently, and different content is a conflict. Exit
status is nonzero when any file fails.

Supported storage drivers and their settings are documented in
[`docs/CONFIG.md`](docs/CONFIG.md). Pinecone uses an existing dense serverless
index: `storage.url` is the index host, `storage.pinecone.api_key` is its API
key, and `storage.pinecone.namespace` is an optional namespace prefix. Each
configured collection gets its own Pinecone namespace. ScreenJSON records
without embeddings receive zero vectors with a metadata flag so normal reads
and writes retain the original record shape.
