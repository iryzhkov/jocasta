# Jocasta development

Use Huyang for repository reads and edits. Run `make check test cross` before
publication. CLI and server share `pkg/api`; changes must preserve the versioned
wire contract or introduce an explicit version change.

Document content is exact UTF-8, never reformatted. Revisions and idempotency
receipts are transactional. Do not bypass optimistic concurrency, silently retry
conflicts, delete history, or infer a write target from approximate search.

Live SQLite WAL storage must use a supported local filesystem. NAS storage is for
SQLite-aware backups. Deployment tooling owns server configuration, CLI installation and per-host
credential references. Never put credentials in source,
logs, release manifests, or test evidence.

The accepted retention policy archives documents after 30 days without a read or
update. Archived documents remain searchable by title/path and retrievable.
A source-file migration preserves original files as read-only snapshots after verified
cutover. Until cutover is verified, do not describe the service as authoritative.
