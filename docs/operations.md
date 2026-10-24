# Operations

Deployment configuration, TLS ingress, credentials and backup units are managed
outside this source repository. Keep per-host credentials private and distribute
the CLI through your normal installation mechanism.

## Command instrumentation

Each run of the `jocasta` command appends one JSON line to
`~/.local/share/toolfeedback/jocasta/<host>-<date>.jsonl`, the same spool the
fleet's MCP servers write and that operational reporting tools can read. The
line holds the verb, the flag keys, an outcome class, the exit code and the
elapsed milliseconds, and it is what makes a per-verb failure rate a
measurement rather than an impression.

It carries no argument values. The verb is recorded only when it names a help
topic this program publishes, a flag key only when the parser's own flagset
declares it (anything else is recorded as `?`), and everything after a bare
`--` is not scanned at all. An error is recorded as a class from a closed
vocabulary, never as its message, because a message quotes the document, the
path or the query that produced it and these records are shipped to another
host. `outcome` is one of `ok`, `usage` (exit 6 and exit 4), `state` (2, 3 and
7), `transport` (5 when the server did not answer) and `internal` (exit 5 when
nothing classified the failure).

The spool is off unless it was asked for: create the file `enabled` in the
spool directory, or set `JOCASTA_FRICTION=1`, or set `TOOLFEEDBACK=1` to move
every tool together; either variable set to `0` turns it off again.
`JOCASTA_FRICTION_DIR` or `TOOLFEEDBACK_DIR` moves the directory. A spool that
cannot be written is skipped in silence and never changes the exit code,
standard output or standard error.

## Storage, credentials and retention

Use local ext4/btrfs storage for live SQLite WAL. Do not put the live database on
NFS/SMB. The container runs as UID 10001 and requires writable `/data`; mount a
read-only JSON host-to-token mapping at `/run/secrets/jocasta-tokens.json`.
Bind the backend so only the configured TLS ingress can reach it. Removing a host
from the token map revokes that credential on its next request. Rotate one host
at a time, provisioning its owner-only token file before switching its server
entry. Never print tokens or add them to a release manifest.

Configuration: `JOCASTA_DB`, `JOCASTA_LISTEN`, `JOCASTA_TOKENS_FILE`,
`JOCASTA_MAX_DOCUMENT_BYTES`, `JOCASTA_MAX_STORAGE_BYTES`,
`JOCASTA_MIN_FREE_BYTES`, `JOCASTA_ARCHIVE_AFTER_DAYS`.
Optional project-scoped JWT reads require all three of `JOCASTA_OIDC_ISSUER`,
`JOCASTA_OIDC_AUDIENCE` and `JOCASTA_OIDC_JWKS_URL`. To enable the
HTTP MCP adapter, also set `JOCASTA_MCP_RESOURCE_URL` to its exact canonical
HTTPS `/mcp` URL. Keep the existing static token file for operator REST access;
static tokens are not accepted by MCP. See [HTTP MCP](http-mcp.md) for trust,
ingress and claim requirements. Both optional authentication paths are disabled
by default.

Defaults reserve 1 GiB free space, cap storage at 10 GiB, and archive after
30 inactive days. Monitor capacity and errors; no historical deletion occurs.

The server periodically archives inactive documents. Explicit reads and updates
refresh activity; listing/searching alone do not. Archival creates a revision,
retains identity/history, and keeps names searchable. It does not delete content.

## Backup and recovery

Use `jocastad backup --db /data/jocasta.db --output /backup/new.db` for an online
consistent SQLite snapshot. The output must be new. Never copy a live database
file without its WAL through ordinary filesystem copy.

The reference backup policy takes verified hourly snapshots to separate storage,
retaining 48 hourly and 30 daily points. A missing mount must fail; it must not write into an underlying
local directory. Failed/stale backups are operational failures. The intended
healthy-backup RPO is one hour and operator-led RTO four hours; outages or full
storage can exceed those objectives.

To recover, stop writes, preserve the damaged database and its WAL, select and
record a verified backup timestamp, and restore into an empty isolated instance.
Run SQLite integrity checks, verify known IDs/revisions/aliases and receipt
replays, then `jocastad reindex --db RESTORED_DB` and test search. Reconcile writes
newer than the recovery point before replacing production data. Report that loss
window explicitly. Immutable revisions do not eliminate backup loss.

Before any schema upgrade, take and verify an online backup. Older binaries
refuse newer schemas; do not force a downgrade. Rolling back a binary does not
roll back data.

## Import from existing files

Do not import until both service recovery and fleet fetch/search have passed.
Freeze the source against all writers, record the freeze timestamp, and preserve
it afterward as a legacy snapshot.

```sh
jocasta import /path/to/source/plans --dry-run --migration-id plans-import-EXAMPLE --manifest /tmp/jocasta-import.json --prefix legacy
jocasta import /path/to/source/plans --migration-id plans-import-EXAMPLE --manifest /tmp/jocasta-import.json --json
```

The manifest records every source path, destination, byte count/hash or skipped
reason. Review mapping before execution: mixed files belong in `legacy/`; use
project paths only when unambiguous. The import checks the inventory before and
after, never overwrites existing destinations, and verifies exact pinned content.
Repeated imports with the same manifest and migration ID reuse receipts.
Keep the manifest and returned document IDs/revisions/hashes as migration evidence.

Binary archives, symlinks, non-regular files, invalid UTF-8/NUL and oversized files
are excluded with reasons. They remain at the source; they are not silently lost.
Do not delete original files. Cutover becomes authoritative only after verifying
counts and hashes and publishing shared agent instructions. If the source cannot
be frozen, stop before cutover.

Before cutover rollback keeps the legacy folder authoritative. After new service
writes begin, export/reconcile them before considering any rollback to legacy
files. Never silently fall back to dual mutable stores.


## Interactive MCP operator trust

Optional `JOCASTA_OPERATOR_OIDC_ISSUER`, `JOCASTA_OPERATOR_OIDC_JWKS_URL`
and `JOCASTA_OPERATOR_OIDC_CLIENT_ID` configure a separate pinned operator
verifier. All default empty and must be supplied together with existing limited
OIDC and the canonical MCP URL. Corresponding flags are
`--operator-oidc-issuer`, `--operator-oidc-jwks-url`,
`--operator-oidc-client-id`. No secret belongs in these settings.
See [HTTP MCP](http-mcp.md) for claims, catalogs, bounds and native acceptance.

Stage with operator trust off, enroll the dedicated public OAuth application
through owning infrastructure, then enable only its exact issuer/JWKS/client ID.
Use supported native OAuth login; do not copy static operator tokens or share
limited-session helper credentials. Verify real claim behavior, read/write
refusals, CAS/idempotency and automatic refresh separately for each pinned native
client and official T3 composition. Keep named limited profiles limited.

Rollback disables the new registration, revokes its refresh grants/application,
then removes operator trust at the server and retires only owned issuer objects.
Existing limited provider, REST tokens and documents remain. Removing issuer
trust ends server acceptance; revoking issuance alone leaves already issued JWTs
valid for up to expiry plus the existing 30-second leeway. No schema rollback is
needed. Reverting code does not undo legitimate edits; any content restoration
uses ordinary revisioned CAS operations.
