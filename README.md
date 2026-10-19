# Jocasta

`jocasta --describe` prints compact JSON discovery metadata (`name`, `summary`,
`help`, and `commands` with `name`, `summary`, `access`). It reads no
configuration or credentials, makes no requests, and skips telemetry.
Access is `read` or `write`; document reads that reset retention activity,
and reads that can write an output file, are conservatively `write`.


Jocasta stores shared working documents for the agent fleet: plans, handoffs,
research and prompts. A Go CLI talks to one authenticated HTTP API behind private
TLS ingress. The server preserves exact UTF-8 bytes and immutable revisions in
SQLite, with optimistic concurrency, path aliases, lexical search and backups.

## Use

Install `jocasta` and place its configuration in `~/.config/jocasta/config.json`.
Credentials stay in a private per-host token file; do not place them in arguments,
environment variables, Git or handoffs.

```sh
jocasta search 'rough plan name' --json
jocasta get project/plans/design.md --output /tmp/design.md --json
jocasta put project/plans/design.md --file /tmp/design.md --if-revision 3
jocasta put project/handoffs/next.md --file /tmp/handoff.md --create
jocasta get jocasta:DOCUMENT_ID@3
jocasta exists project/inputs/answer.md --json
jocasta history --id DOCUMENT_ID --json
jocasta mv project/plans/design.md project/plans/revised.md --if-revision 4
jocasta archive --id DOCUMENT_ID --if-revision 5
jocasta unarchive --id DOCUMENT_ID --if-revision 6
jocasta restore --id DOCUMENT_ID --revision 3 --if-revision 7
```

`jocasta help` lists the commands; `jocasta COMMAND --help`, `jocasta COMMAND -h` and
`jocasta help COMMAND` print that command's own page, with its flags and their
defaults, what it guarantees, the guard contract for a command that writes, and the
exit codes that command returns. A help page makes no request and needs no token.

A conflict requires a fresh read and deliberate reconciliation. The refusal names
the guard, both revisions, the document, who wrote the newer revision, and the
`get` and `diff` commands that reconcile it. Creation requires `--create`;
updates require a current revision or pinned exact reference. Reusing
`--request-id` after a lost response returns the original receipt; using the same
key for different input fails. Receipts are retained for at least 30 days.

A write answers with the new revision, not with the document. The receipt names
the ID, the revision, the path, the content hash and the line count; the content
itself is read back with `get`, or with `put --verbose`, which fetches the exact
revision it just wrote.

`jocasta diff --from N --to M` compares two revisions and prints a unified diff:
only the changed lines, each in a hunk with up to three lines of context. Its
size follows the change rather than the document, which is what makes it usable
for reconciling a conflict in a large document.

Full `get` writes exact content to stdout. `--json` returns a versioned receipt;
`--output FILE --json` writes the file and emits metadata without its contents.
An existing regular output file is refused unless `--overwrite` is given, which
replaces it atomically; a device such as `/dev/null` is written directly, so a
throwaway read in a script or a wait check exits 0. `get --lines 10:30 --json`
is an explicit partial view and cannot use `--output`. Never use a partial view
as a replacement document.

`jocasta exists PATH` (or `jocasta:ID`, `jocasta:ID@REV`) answers presence
alone, without fetching or writing content: exit 0 when the document is
present, 2 when it is absent, and the usual 4 and 5 for authentication and an
unavailable service. With `--json` it prints
`{"version":"jocasta/v1","exists":true|false,"document":{id,path,revision,archived}}`.
Use it as the condition of a wait check instead of `get --output /dev/null`.

Search supports title/path/alias matching, bounded typo tolerance, FTS token
matching and `--mode literal` for punctuation-rich text. Inspect `has_more`,
`next_cursor` and `complete`, and continue with `--cursor`. A zero-match
incomplete page is not proof of absence. Concurrent mutations invalidate list and
search cursors. Search cannot promise conceptual matches without shared words.

Documents automatically archive after 30 days without a read or update. Archived
documents stay searchable by title/path and fully retrievable; use
`--include-archived` for their content. Reading updates activity but does not
unarchive. Explicit unarchive starts a fresh inactivity window. History is never
automatically deleted.

## Configure and develop

```json
{"url":"https://jocasta.example.com","token_file":"/path/to/private/jocasta-token"}
```

An optional `ca_file` adds a private CA. HTTPS certificate verification is always
enabled. Redirects are refused. HTTP requires explicit `allow_loopback_http:true`
and a loopback address, for local development only.

```sh
make build
make check test cross
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT=SOURCE_COMMIT -t jocasta:SOURCE_COMMIT .
```

The Docker server runs as UID 10001. Mount local persistent data at `/data` and an
owner-restricted token JSON map at `/run/secrets/jocasta-tokens.json`. Each key is a
host actor; each value is a distinct random bearer token of at least 32 bytes.
The server reloads credentials per request, so removing a host revokes access.
The HTTP listener is for a restricted TLS ingress, not direct public exposure.

An opt-in [HTTP MCP reader](docs/http-mcp.md) exposes get/list/search/history with
per-request project grants and a resource-specific JWT audience. It is disabled
by default; Authentik resource audience and client/T3 runtime qualification remain pending.

See [the operator guide](docs/operations.md), [API contract](docs/api.md), and
[original design](docs/design-plan.md). The current 30-day archive policy and
Docker deployment supersede the original plan's defaults. The design document records the original implementation contract.

## Ownership

Jocasta owns working documents. Git and Huyang own repository source. OpenViking
holds curated durable knowledge only when explicitly requested. Scheduling and execution belong to external tools; Jocasta does not run tasks.
Deployment owners manage server configuration, TLS, credentials, backups and CLI distribution.
