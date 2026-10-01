# Configuration

Generated from `internal/config` by `go run ./internal/config/cmd/configdoc`; a test keeps it in step with the code.

Settings come from, highest precedence first: command-line flags (`--set path=value`), environment variables, the YAML file (`--config` or `SCREENJSON_CONFIG`), and the defaults below. `${VAR}` and `${VAR:-default}` in the YAML file are filled from the environment.

| Setting | Environment | Default | Meaning |
|---|---|---|---|
| `mode` | `SCREENJSON_MODE` | `full` | full serves the web UI and the API; headless serves only the API (15.5). |
| `data_dir` | `SCREENJSON_DATA_DIR` | `/data` | Where the state store (state.db) and Git work copies live. |
| `log.level` | `SCREENJSON_LOG_LEVEL` | `info` | debug, info, warn or error. |
| `admin.key` | `SCREENJSON_ADMIN_KEY` | `(empty)` | Allows editing the configuration through PATCH /config (and the Status page), sent as the X-Admin-Key header. Empty means the configuration is read-only there. Set it in the environment or the file; it can't be changed through the API. |
| `http.addr` | `SCREENJSON_HTTP_ADDR` | `127.0.0.1:8080` | Listen address. The container image sets 0.0.0.0:8080. |
| `http.cors_origins` | `SCREENJSON_HTTP_CORS_ORIGINS` | `[]` | Origins allowed by CORS, for headless mode with another UI. Comma-separated in the environment. |
| `http.require_token` | `SCREENJSON_HTTP_REQUIRE_TOKEN` | `false` | Refuse requests without a token with 401 (R-TOK-04). |
| `http.require_if_match` | `SCREENJSON_HTTP_REQUIRE_IF_MATCH` | `false` | Refuse writes without If-Match with 428. |
| `http.max_body` | `SCREENJSON_HTTP_MAX_BODY` | `32MiB` | Largest request body; larger ones get 413. |
| `http.mcp` | `SCREENJSON_HTTP_MCP` | `true` | Serve the Model Context Protocol endpoint at /mcp: the API as tools for LLMs, with the caller's token. |
| `storage.driver` | `SCREENJSON_STORAGE_DRIVER` | `memory` | memory, mongo, postgres, elastic, chroma, weaviate or pinecone. |
| `storage.url` | `SCREENJSON_STORAGE_URL` | `(empty)` | Connection URL for the driver. |
| `storage.database` | `SCREENJSON_STORAGE_DATABASE` | `(empty)` | Database name, where the driver has one. |
| `storage.transactions` | `SCREENJSON_STORAGE_TRANSACTIONS` | `(driver default)` | Wrap multi-record writes in a transaction. Defaults to true on mongo and postgres, and is unavailable elsewhere. |
| `storage.envelope_field` | `SCREENJSON_STORAGE_ENVELOPE_FIELD` | `sj` | Name of the envelope field on mongo and elastic records. |
| `storage.allow_large_records` | `SCREENJSON_STORAGE_ALLOW_LARGE_RECORDS` | `false` | Let chroma, weaviate and pinecone store a whole screenplay in one record (R-DRV-07). |
| `storage.layout` | `SCREENJSON_STORAGE_LAYOUT` | `elements` | A preset name (whole, scenes, elements) or a map of levels. In the environment, a preset name or a JSON layout. |
| `storage.elastic.refresh` | `SCREENJSON_STORAGE_ELASTIC_REFRESH` | `wait_for` | Elastic refresh policy for writes: wait_for, true or false. |
| `storage.chroma.tenant` | `SCREENJSON_STORAGE_CHROMA_TENANT` | `default_tenant` | Chroma tenant. |
| `storage.chroma.database` | `SCREENJSON_STORAGE_CHROMA_DATABASE` | `default_database` | Chroma database. |
| `storage.weaviate.class_prefix` | `SCREENJSON_STORAGE_WEAVIATE_CLASS_PREFIX` | `Sj` | Prefix for Weaviate class names. |
| `storage.pinecone.api_key` | `SCREENJSON_STORAGE_PINECONE_API_KEY` | `(empty)` | Pinecone API key. |
| `storage.pinecone.namespace` | `SCREENJSON_STORAGE_PINECONE_NAMESPACE` | `(empty)` | Optional prefix for the namespaces used for server collections. |
| `cache.max_documents` | `SCREENJSON_CACHE_MAX_DOCUMENTS` | `256` | Most documents held in memory. |
| `cache.max_bytes` | `SCREENJSON_CACHE_MAX_BYTES` | `1GiB` | Most bytes of documents held in memory. |
| `queue.length` | `SCREENJSON_QUEUE_LENGTH` | `1000` | Operations a document's write queue holds before answering 429 (R-ARCH-04). |
| `checkouts.default_ttl` | `SCREENJSON_CHECKOUTS_DEFAULT_TTL` | `5m0s` | Checkout lifetime when none is asked for. |
| `checkouts.max_ttl` | `SCREENJSON_CHECKOUTS_MAX_TTL` | `1h0m0s` | Longest checkout lifetime allowed. |
| `numbering.default` | `SCREENJSON_NUMBERING_DEFAULT` | `none` | Scene numbering when a document sets none in meta: none, auto or frozen (7.4). |
| `events.backend` | `SCREENJSON_EVENTS_BACKEND` | `embedded` | embedded (Centrifuge inside the server) or centrifugo (an external server). |
| `events.history_size` | `SCREENJSON_EVENTS_HISTORY_SIZE` | `1000` | Events kept per document channel for recovery. |
| `events.history_ttl` | `SCREENJSON_EVENTS_HISTORY_TTL` | `10m0s` | How long events are kept for recovery. |
| `events.centrifugo.api_url` | `SCREENJSON_EVENTS_CENTRIFUGO_API_URL` | `(empty)` | Centrifugo HTTP API URL. |
| `events.centrifugo.api_key` | `SCREENJSON_EVENTS_CENTRIFUGO_API_KEY` | `(empty)` | Centrifugo API key. |
| `events.centrifugo.client_url` | `SCREENJSON_EVENTS_CENTRIFUGO_CLIENT_URL` | `(empty)` | Websocket URL browsers connect to. |
| `webhooks` | YAML only | `[]` | Webhook receivers (section 9). YAML only. |
| `git.enabled` | `SCREENJSON_GIT_ENABLED` | `false` | Turn Git export on. |
| `git.mode` | `SCREENJSON_GIT_MODE` | `repo-per-doc` | repo-per-doc or mono. |
| `git.workdir` | `SCREENJSON_GIT_WORKDIR` | `(empty)` | Where local work copies live. Defaults to data_dir/git. |
| `git.remote` | `SCREENJSON_GIT_REMOTE` | `(empty)` | Remote URL; {doc} and {slug} are filled in per document in repo-per-doc mode. |
| `git.branch` | `SCREENJSON_GIT_BRANCH` | `main` | Branch to commit to. |
| `git.push` | `SCREENJSON_GIT_PUSH` | `true` | Push after each commit. |
| `git.author.name` | `SCREENJSON_GIT_AUTHOR_NAME` | `screenjson-server` | Commit author name. |
| `git.author.email` | `SCREENJSON_GIT_AUTHOR_EMAIL` | `screenjson@localhost` | Commit author email. |
| `git.include_analysis` | `SCREENJSON_GIT_INCLUDE_ANALYSIS` | `false` | Include analysis in committed files. |
| `git.auto_commit_idle` | `SCREENJSON_GIT_AUTO_COMMIT_IDLE` | `0s` | Commit a document after this long without writes; 0 turns it off. |
| `git.ssh_key` | `SCREENJSON_GIT_SSH_KEY` | `(empty)` | Path to an SSH private key for pushing. |
| `git.credentials` | YAML only | `[]` | Accounts for pushing each script's remotes (GitHub, GitLab, Bitbucket), by name. YAML only; put tokens in with ${VAR}. |
| `git.token` | `SCREENJSON_GIT_TOKEN` | `(empty)` | HTTPS token for pushing (SCREENJSON_GIT_TOKEN). |
| `blob.driver` | `SCREENJSON_BLOB_DRIVER` | `fs` | s3, minio, azure or fs. |
| `blob.endpoint` | `SCREENJSON_BLOB_ENDPOINT` | `(empty)` | Endpoint URL, for minio or another S3-compatible store. |
| `blob.region` | `SCREENJSON_BLOB_REGION` | `us-west-2` | S3 region. |
| `blob.bucket` | `SCREENJSON_BLOB_BUCKET` | `(empty)` | Default bucket, used when a target URL names none. |
| `blob.access_key` | `SCREENJSON_BLOB_ACCESS_KEY` | `(empty)` | S3 or MinIO access key; the Azure account name for azure. |
| `blob.secret_key` | `SCREENJSON_BLOB_SECRET_KEY` | `(empty)` | S3 or MinIO secret key; the Azure account key for azure. |
| `blob.root` | `SCREENJSON_BLOB_ROOT` | `(empty)` | Base directory for the fs driver. |
| `greenlight.servers` | `SCREENJSON_GREENLIGHT_SERVERS` | `[]` | Up to 5 Greenlight servers as host:port. Jobs are spread over the ones that are up, fewest unfinished jobs first; a server that fails to take one is skipped. Comma-separated in the environment. |
| `greenlight.tls` | `SCREENJSON_GREENLIGHT_TLS` | `false` | Talk to the Greenlight servers over HTTPS. |
| `greenlight.api_key` | `SCREENJSON_GREENLIGHT_API_KEY` | `(empty)` | Greenlight's SHARED_API_KEY, sent as a bearer token. |
| `greenlight.check_interval` | `SCREENJSON_GREENLIGHT_CHECK_INTERVAL` | `15s` | How often each Greenlight server is pinged. |
| `greenlight.handover` | `SCREENJSON_GREENLIGHT_HANDOVER` | `storage` | How a job's input reaches Greenlight: storage (written to its input bucket) or upload (its POST /upload). |
| `greenlight.s3.endpoint` | `SCREENJSON_GREENLIGHT_S3_ENDPOINT` | `(empty)` | S3 API URL for MinIO or another S3-compatible store; empty for AWS S3. |
| `greenlight.s3.region` | `SCREENJSON_GREENLIGHT_S3_REGION` | `us-east-1` | S3 region. |
| `greenlight.s3.access_key` | `SCREENJSON_GREENLIGHT_S3_ACCESS_KEY` | `(empty)` | Access key. |
| `greenlight.s3.secret_key` | `SCREENJSON_GREENLIGHT_S3_SECRET_KEY` | `(empty)` | Secret key. |
| `greenlight.s3.bucket` | `SCREENJSON_GREENLIGHT_S3_BUCKET` | `(empty)` | Greenlight's S3_INPUT_BUCKET; read from Greenlight's /setup when empty. |
| `greenlight.s3.prefix` | `SCREENJSON_GREENLIGHT_S3_PREFIX` | `(empty)` | Greenlight's S3_INPUT_PREFIX; read from Greenlight's /setup with the bucket. |

## Webhook entries

Each entry under `webhooks` has `name`, `url`, `on` (a list of events), and optionally `secret` (signs deliveries with HMAC-SHA256), `kinds` and `types` (filters).

## CLI aliases

The `screenjson` CLI's own variables are accepted with the same meaning. The server's names win when both are set.

| CLI variable | Setting |
|---|---|
| `SCREENJSON_AWS_ACCESS_KEY` | `blob.access_key` |
| `SCREENJSON_AWS_REGION` | `blob.region` |
| `SCREENJSON_AWS_SECRET_KEY` | `blob.secret_key` |
| `SCREENJSON_BLOB_BUCKET` | `blob.bucket` |
| `SCREENJSON_BLOB_TYPE` | `blob.driver` |
| `SCREENJSON_DB_TYPE` | `storage.driver` |
| `SCREENJSON_DB_HOST`, `_PORT`, `_USER`, `_PASS` | build `storage.url` when it is not set |
