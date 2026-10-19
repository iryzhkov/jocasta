# Jocasta: shared working documents for agents

Status: original design contract, retained for architecture context.
Prepared: 2026-09-15. Deployment-specific records are kept privately.

## Outcome

Replace host-specific access to an existing local plans directory with one authoritative document service and a CLI available on every agent host. Support plans, handoffs, research, prompts, and other large UTF-8 text files. A user can name a document approximately or remember a phrase and an agent can discover it without SSH or knowing its storage host.

This is working-document storage. The external scheduler remains responsible for scheduling and execution state. OV remains curated long-term knowledge, written only on explicit user request. No automatic mirroring into OV, no new task runner, and no MCP interface in v1.

## Agreed scope and proposed defaults

Agreed: one server, remote CLI, project folder paths, search, versioning, large text documents, no semantic search for now.
Proposed implementation defaults: a Go service and CLI in one repository; SQLite with FTS5 on local disk; authenticated HTTPS over the existing private network; systemd service on the chosen host, subject to checking the actual supported deployment environment. Pin a SQLite driver with FTS5 support and prove its build on fleet platforms before committing to it. Confirm repository owner, service address, storage directory, backup destination and existing fleet secret distribution during implementation discovery; do not invent infrastructure facts.

Do not build embeddings, LLM summaries, vector search, a browser UI, collaborative live editing, offline write queues, multi-server replication, binary attachments, or public sharing in v1. Git export may come later; SQLite revisions are the authority.

## Document model

- A document has an opaque stable ID, a current canonical path, optional explicit title and kind, archive state, and a monotonically increasing revision number.
- Paths are relative, case-sensitive UTF-8 strings such as huyang/plans/usability.md and huyang/handoffs/round-two.md. The first segment is a project namespace. Normalize Unicode to NFC, accept '/' separators only, and reject empty segments, '.', '..', leading/trailing '/', control characters and reserved internal namespaces. Define byte length limits in the API. Paths are logical keys and never become filesystem paths.
- Each revision stores a complete immutable snapshot of content AND metadata, including the path, archive state, title and kind, plus server timestamp, authenticated actor, content hash and optional change message. Identical retries do not produce new revisions. A newly requested update with no changes returns the existing revision.
- Stable identity survives move, archive and restore. All mutations increment the same revision counter; metadata-only changes participate in concurrency checks. Moves reserve the old path as an alias to that ID, preventing accidental reference reuse. Alias resolution is indicated in output; listing/search normally display only the current path. Moving back to one's own alias is allowed atomically. Cross-document path/alias collisions fail. Hard deletion and alias reclamation are out of scope.
- Archive is reversible, hides the document from default listing/search, and does not break explicit retrieval by path or ID. An explicit archived retrieval clearly reports archive state.
- Preserve valid UTF-8 bytes exactly, including CRLF, trailing newline and whitespace. Reject invalid UTF-8 and NUL with actionable errors; do not normalize content or regenerate it through a model. Initial configurable maximum: 16 MiB per document. Stream request bodies and enforce bounds before allocating unbounded buffers. Disk quota/storage-full errors must not leave partial revisions.

## CLI contract (illustrative spelling)

    jocasta put huyang/plans/usability.md --file plan.md --create
    jocasta get huyang/plans/usability.md --output plan.md --json
    jocasta put huyang/plans/usability.md --file plan.md --if-revision 7 --message 'Clarify checks'
    jocasta ls huyang/plans/
    jocasta search 'agent usability workshop' --project huyang
    jocasta history huyang/plans/usability.md
    jocasta get --id DOC_ID --revision 7
    jocasta diff --id DOC_ID --from 6 --to 7
    jocasta mv huyang/plans/usability.md huyang/plans/editing.md --if-revision 7
    jocasta archive --id DOC_ID --if-revision 8
    jocasta unarchive --id DOC_ID --if-revision 9
    jocasta restore --id DOC_ID --revision 7 --if-revision 10

`put --file -` accepts stdin. Creation requires --create and fails if the path or alias exists. Updates require --if-revision; no unconditional overwrite flag in v1. A conflict reports expected/current revision and document ID, exits distinctly, and never retries by replacing newer content. Agents re-read and reconcile intentionally. Authenticated principal identifies the actor; host/session attribution can be supplied as untrusted provenance.

`get` emits exact full content to stdout by default, with diagnostics on stderr. `--json` emits a versioned JSON envelope including ID, requested reference, canonical path, revision, content hash, metadata and content. With --output FILE --json, write content to the file and return its metadata receipt on stdout, without duplicating content. File output refuses existing files unless --overwrite is explicit. No command infers a write target from fuzzy search results.

`restore` copies the chosen revision's content/title/kind into a NEW current revision; it retains the current path and archive state. Historical moves are not implicitly undone. Use mv/unarchive explicitly when needed.

Provide bounded line-range retrieval (`get --lines 100:200`) with snapshot revision and total line count in JSON. Line numbers are 1-based and ranges inclusive. Empty content has zero lines; a final newline terminates its line without creating a phantom extra line. Reject nonpositive/reversed ranges and starts beyond EOF; clamp an end beyond EOF and report requested and returned ranges. The full_document_hash always hashes complete original bytes, never the excerpt. Full get is exact; range reads require --json in v1, with partial=true and the source revision/range; disallow --output with --lines. Only full retrieval writes raw files. A range is explicitly a partial view and must never be mistaken for content suitable for replacing the complete document. Heading retrieval is optional follow-up, not an acceptance gate.

List/history/search use bounded limits and opaque cursors; use a durable global mutation generation: list/search cursors bind filters, ordering and generation, and fail with cursor_expired if any write changes that generation; the client restarts explicitly. History uses immutable revision keyset pagination anchored to the initial maximum revision. Reject malformed/mismatched cursors. JSON includes next_cursor and explicit truncation indicators where applicable. Human output includes short IDs and revisions. Set stable exit codes for success, not found, conflict, auth failure, unavailable, invalid input and local I/O failure. CLI requests have timeouts, bounded retries for safe reads and idempotent writes, and useful stderr errors without content/credential leaks.

## Search without semantic search

Default search covers only latest, unarchived revisions. No embeddings or external model dependency.

1. Match canonical paths, aliases and explicit/derived titles: exact first, then prefix/substring, then bounded typo tolerance. Derive display title from the first Markdown heading or filename when no explicit title exists, capped at 256 Unicode code points, with a bounded 64 KiB heading scan; never modify stored content.
2. Use SQLite FTS5 over title/path/content for token and phrase matching. Boost title/path matches; return bounded relevant excerpts with document ID, path, revision and modification time.
3. Merge/deduplicate by ID with deterministic ordering and pagination. Cap fuzzy candidate expansion and query length; measure on the representative corpus so fuzzy matching does not scan all large content on each query.

Offer explicit literal/phrase search for punctuation-rich identifiers, filenames, code and URLs that tokenization can miss; scan current eligible documents in stable ID order with a work budget and continuation cursor, without a preceding FTS candidate filter. Report scanned_count, has_more and complete explicitly: a page with zero matches is not an exhaustive negative when has_more=true. Concurrent writes invalidate the scan cursor as above. Fuzzy candidate-limited results likewise report completeness and the applied limit; never present a capped result as proof of absence. Normal queries are treated as user text, not executable FTS syntax or SQL. Filters include project/path prefix, kind and --include-archived. Historical revision search is deferred; history/get/diff provide historical access.

Approximate naming is a supported goal, but conceptual paraphrases with no shared terms are not guaranteed without semantic search. Results show enough context for agents to disambiguate; never auto-execute against a guessed match.

## API, persistence and concurrency

Expose a versioned HTTP API used identically by every CLI. Separate explicit path references from ID references; historical retrieval resolves the ID then fetches that document's snapshot. Exact references are the normal handoff format: `jocasta:<document-id>@<revision>`; read commands accept this exact reference. For mutations, a pinned reference supplies the expected CURRENT revision, never permission to mutate a historical snapshot; reject it if stale. If --if-revision is also supplied it must agree. Restore uses an unpinned target ID plus a separate source --revision and current --if-revision. get rejects contradictory reference/--revision arguments. Include canonical path as a human hint, not identity.

SQLite owns documents, immutable revisions, unique path/alias reservations, idempotency receipts and the current search projection. A write validates authentication, request and expected revision, then commits the new snapshot, current pointer, path reservations and index projection in one transaction. Use WAL with synchronous=FULL on a supported local filesystem, acknowledge only after commit, and use bounded busy waits. Process-crash recovery must preserve every acknowledged write; hardware/filesystem durability remains an explicit deployment assumption. A database-level compare-and-swap/transaction protects against two clients updating the same revision. Failed indexing must roll back the write; search must never silently serve an older revision after acknowledgement. Index reconstruction from authoritative revisions is supported and preserves identity/history.

Each mutating request carries an idempotency key scoped to its authenticated principal. Persist payload digest and response atomically with the write; reusing a key for different input is an error. Keep receipts at least 30 days and document the retry window. A retry after a lost response returns the original result within that window, even if the document has since changed. CLI generates a key per invocation, reuses it for retries and accepts --request-id for operator-controlled recovery. After expiration, creation/existing-path and revision checks still prevent silent duplicate overwrites.

Bind to a private interface or loopback behind the authenticated TLS ingress; require authentication on all document/search APIs. Use per-host revocable credentials, stored through existing fleet secret references, with a single trusted-user permission scope initially. No promise of project-level access isolation. Never log authorization headers or document bodies. Enforce body/query/time limits and rate limits suitable for the private fleet. Database files/backups contain sensitive project text and require restrictive permissions. CLI verifies TLS certificates (using fleet/private CA where appropriate), rejects non-HTTPS endpoints except explicit loopback development mode, and never forwards credentials across redirects. The service validates per-host bearer tokens itself; ingress supplies TLS only and forwarded identity headers have no authority. Restrict direct service access to the local ingress. Actor identifies a host credential, not an independently authenticated person/agent. Configure aggregate database/storage budgets, reserved disk headroom and backup capacity during deployment; monitor database/WAL/index/receipt growth, cap concurrency and expensive work, and reject new writes before exhausting the host. Return storage_full without a partial revision. No automatic historical deletion in v1.

Use SQLite-aware online backups, not a raw copy of a live WAL database. Proposed operational objectives: hourly off-host backups with 48 hourly and 30 daily recovery points, RPO at most one hour when backups are healthy, and operator-led RTO four hours. Validate these against the existing backup destination and alert on failed/stale backups. Ordinary backup restore may lose acknowledged writes newer than the recovery point: report the chosen recovery timestamp/loss window, retain the damaged database, and reconcile clients before resuming writes. A rollback must never imply zero loss merely because revisions are immutable. Test recovery onto a clean instance, including IDs, revisions, aliases, receipts and search reconstruction. Include schema migration versioning, backup-before-migration, and explicit refusal to run older binaries against newer unsupported schemas. Health/readiness, request/error metrics and disk usage should allow diagnosing an unavailable store without reading documents.

## Migration and rollout

1. Discover fleet conventions, deployment owner, supported platforms, existing backup mechanism and secrets. Inspect owning repository instructions through Huyang before source work. Check repository/binary collisions before installation; preserve the selected project name Jocasta.
2. Build and validate a local service+CLI slice: create, read, revision-checked update, conflict, history and exact byte preservation.
3. Add path moves/aliases, archive/restore, ranges, bounded list/history, search and idempotency. Validate real plan/handoff examples and synthetic large files.
4. Deploy a staging instance with persistent storage; validate from two actual client hosts, TLS/auth, revocation, timeouts, backups and recovery. Pick production defaults from measured evidence.
5. Create a dry-run import manifest for the source plans directory. Inventory paths, sizes and content hashes; refuse symlinks, non-regular files, invalid content and oversized files, reporting skipped reasons. Do not execute file contents. Review project mapping: existing mixed files go into an explicit `legacy/` namespace unless their project is unambiguous. Record every source path -> destination path -> document ID/revision/hash. Import keys derive from migration ID + source relative path + content digest. Changed sources after inventory fail verification and require a fresh manifest; never overwrite a destination by default. Repeated imports return the existing successful receipt or report conflicts.
6. Verify destination bytes/hashes and counts against the manifest, report every exclusion, and preserve the source directory. Establish a coordinated source write freeze before the final comparison/import, covering interactive agents and running/queued tasks that reference the old folder. Announce the authoritative location and keep the source read-only through final verification and cutover. If a freeze cannot be established, stop migration before cutover. Resolve all changes since inventory and record the freeze/cutover timestamps. Declare the service authoritative in shared instructions, with old files explicitly marked legacy snapshots so agents do not write both stores. Do not delete old files as part of migration.
7. Update owning shared instruction/skill sources, regenerate derived files, and publish validated agent-tooling changes through the deployment tooling per global guidance. Commit/push source, confirm exact-commit required CI, publish release artifacts, validate the intended controller installation, review capture for unrelated pins/environment, publish only intended manifest changes, and confirm release CI before fleet convergence. Use the scheduler's wait registration for external waits, not polling loops. Report publication/convergence blockers honestly. This step applies during implementation, not to saving this planning document.

Rollout acceptance requires the intended hosts to fetch/search the same document revision using only the CLI and configured credentials. If the service is down, fail clearly; do not silently fall back to mutable legacy files. Before cutover, rollback leaves the old folder authoritative. After service writes begin, rollback must preserve/export all new revisions and reconcile before reactivating a legacy workflow; simply reverting binaries or pointing at the old folder is not data rollback.

## Acceptance evidence

- Two clients update revision N concurrently: exactly one succeeds; the loser sees a conflict with no lost content. Include metadata/content races and path collision races.
- Lost-response retry returns the original write receipt; same key/different payload fails; no-op update creates no redundant revision.
- Byte-exact round trips cover LF/CRLF, Unicode, empty files, absent final newline and near-limit documents. Oversize/invalid bodies leave no partial data. Range reads are explicit and correct at boundaries.
- ID and historical references survive move/archive/restore; old path aliases work; collisions fail; restore preserves current location/archive state.
- Search finds a typo in a title, a remembered content phrase, a nested project path and a punctuation-rich identifier; filters and archive exclusion work. Ambiguity remains visible. Updated/archived documents cannot leak stale search projection state.
- JSON contracts, exit codes, pagination/truncation and stdout/stderr separation are tested through the CLI. Writes never accept an ambiguous name match.
- Restart/crash recovery, disk-full/transaction failure, rejected credentials, revocation, request limits, schema compatibility and backup restore have meaningful integration coverage.
- Import dry run changes nothing; repeated import is safe; content changes/collisions/skipped files are accounted for; original files remain intact.
- Benchmark at least 10,000 representative documents totaling 1 GiB on intended hardware. Proposed target: p95 default search under 500 ms and metadata reads under 200 ms on the private LAN. Record corpus, warm/cold conditions, concurrency and actual results; revise target explicitly if needed. Bound diff output and work for maximum-sized documents, returning an explicit too-large error rather than exhausting memory.
- Capture a concise operator guide: install/configure, credentials/rotation, reference syntax, conflict reconciliation, import, backup/restore, migration and service recovery. Include an agent example that locates a roughly named plan, fetches it, and writes a revision-checked handoff in its project folder.

## Delivery gates and audit disposition

Gate A: usable local/staging service with exact text storage, IDs/paths, optimistic concurrency, history, authenticated CLI and basic full-text/name search. Gate B: complete document lifecycle, literal search completeness, retry safety and bounded operations. Gate C: verified production recovery, migration and fleet publication. Heading retrieval, advanced fuzzy ranking, dashboards and semantic search are deferred. Each gate produces a runnable result; no production cutover before Gate C checks pass.

The original design review established pinned mutation/reference semantics, a source write freeze, explicit durability and backup-loss objectives, restricted partial reads, bounded search/pagination, TLS authentication boundaries and staged delivery gates.

## Deliverables

Source repository; service and CLI release artifacts; API/JSON contract; schema/migrations; tests and benchmark evidence; deployment/backup configuration; operator and agent instructions; import inventory/receipts; verified CLI publication and per-host convergence evidence. Avoid new features until this end-to-end path works.
