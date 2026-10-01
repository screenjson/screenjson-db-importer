# screenjson-db-importer

`screenjson-db-importer` loads ScreenJSON `.json` files into a database. Create
those files first with [`screenjson-cli`](https://github.com/screenjson/screenjson-cli),
the open-source tool that converts screenplays from PDF or Final Draft (FDX)
into ScreenJSON. This importer does not convert PDFs or FDX files itself.

This is a standalone open-source command-line tool. Build it from this
repository:

```sh
go build -o screenjson-db-importer .
```

Configure the database and source storage in a YAML file, then import a file,
directory, glob, or configured `file://`, `s3://`, or `azure://` URI:

```sh
./screenjson-db-importer --config importer.yaml --workers 8 \
  --manifest import-state.jsonl --retry-failed ./converted/
```

Preflight validates each source against the ScreenJSON schema before writing.
The importer rereads sources during processing and checks their SHA-256 hashes.
The JSONL manifest records each file's result and lets you resume successful
imports. Identical document IDs and content are treated as already imported;
the same ID with different content is an error. The process exits nonzero if
any file fails. Concurrent imports are refused while another ScreenJSON writer
holds the database state lock.

Supported database and blob storage drivers, their YAML settings, and complete
error behavior are documented in [`docs/CONFIG.md`](docs/CONFIG.md).
Pinecone uses an existing dense serverless index: `storage.url` is the index
host, `storage.pinecone.api_key` is its API key, and
`storage.pinecone.namespace` is an optional namespace prefix. Each configured
collection gets its own Pinecone namespace. Records without embeddings receive
zero vectors with a metadata flag, preserving their ScreenJSON record data.
