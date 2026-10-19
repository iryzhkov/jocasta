# API v1

Every document endpoint requires `Authorization: Bearer HOST_TOKEN`. The server
authenticates it directly; forwarded identity headers have no authority.
All replies use `version: "jocasta/v1"`; errors wrap `{error:{code,message,...}}`.
The Go wire types in `pkg/api/types.go` are the executable schema.

- `POST /v1/mutate`: `Idempotency-Key` plus JSON `operation` (`create`,
  `put`, `move`, `archive`, `unarchive`, `restore`), explicit `path` or
  `id`, and `if_revision` for updates. Creation supplies `content`; move
  supplies `destination`; restore supplies `source_revision`. Optional
  `title`, `kind` and `message` are bounded. Receipts persist atomically with
  snapshots, aliases and the current search index. A mutation receipt answers
  with identity, the new revision, the content hash and the line count, and
  never with the content: the body is not echoed over the wire and not stored a
  second time in the receipts table. Read the bytes back with `GET /v1/get`.
- `GET /v1/get?path=...` or `?id=...`: optional `revision` and `lines=A:B`.
  Receipt includes full content hash, requested reference, alias state, snapshot
  revision and explicit partial/range metadata. Successful reads record activity.
- `GET /v1/list` and `GET /v1/search?q=...`: `project`, `prefix`, `kind`,
  `include_archived`, `limit`, `cursor`; search accepts `mode=literal`.
  Pages carry completeness and continuation metadata. Default search includes
  archived names; archived content requires explicit inclusion. A search page
  reports `ranking`: `bm25` when the full-text index scored it, `literal` for a
  literal scan, or `substring` with a `ranking_reason` when the index could not
  answer and name matching alone ordered the page. Every returned document
  carries its derived title.
- `GET /v1/history?path=...` or `?id=...`: `limit` and `cursor`; history
  pagination anchors to the initial maximum immutable revision.
- `GET /healthz`: unauthenticated liveness only. `/readyz` and `/metrics`
  require authentication and expose no document contents.

Normal search treats user input as text, never raw FTS or SQL syntax. Default
search ranks a document that the query names by its own path or title above one
that merely mentions the same words, then by the index's own relevance, and
breaks ties on recency and path rather than on the random document ID. bm25
scores are relative to the corpus, so the order of results between two documents
can change when other documents are written, removed or reindexed. Cursors bind
filters, ordering and mutation generation; writes make list/search cursors expire.
Paths are NFC-normalized logical keys, never server filesystem paths. Document
content is not normalized. IDs survive moves and archive; old paths remain aliases.
Restore copies historical content/title/kind into a new current revision while
retaining the current path and archive state.

The CLI parses exact `jocasta:ID@REVISION` references. On mutations, the pin is the
expected current revision. Contradictory explicit revision arguments fail locally.
The server always validates the resulting explicit ID/path and revision.

Initial limits are 16 MiB per document, 1,024-byte paths and search queries,
200 results per page, bounded literal scans and fuzzy candidate expansion.
Unsupported schema versions fail closed. All acknowledged mutations use SQLite
WAL with FULL synchronization on supported local storage.

A revision conflict names the guard, both revisions, the document and the actor
that wrote the current revision. The CLI adds the two commands that reconcile it.

Exit codes are defined by the CLI help and tested alongside JSON/stdout separation.
Transport failures are distinct from revision conflicts; the CLI never overwrites
newer content to make a retry succeed. `jocasta diff` compares two revisions in
the client and emits a unified diff whose size follows the change rather than the
document.
