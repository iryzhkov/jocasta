# HTTP MCP documents

Jocastad exposes four read tools at `/mcp`, with two optional operator write tools, using the official
[Go SDK v1.7.0](https://github.com/modelcontextprotocol/go-sdk/tree/bc72835f62eb94d0fb484439f886b6885b075f36).
The annotated tag `25cb00203c6b693780f602ab4041c06f7f4b9570` resolves to
`bc72835f62eb94d0fb484439f886b6885b075f36`. The dependency and its module checksums
are pinned in go.mod/go.sum; its Go requirement is 1.25.0, matching Jocasta.

Disposable Claude and Codex canaries passed the composed Authentik → HTTPS MCP →
official T3 native client → Chaldea result/status/wake path. The Codex run
`e3330681015e4026aaa786a223a31684` exercised 38 calls with allowed-project reads,
denied-sibling reads and filtered enumerations, then completed cleanup. Exact
receipts and remaining limits are recorded in
`jocasta:405dc259d4563fba09a7f2aa9fa6d58e@465` and the authoring guide
`jocasta:38a63cf8918eebae2dd7c8911eacd89c@32`.
These results do not establish persistent deployment, natural renewal during the
composed run, reboot recovery or exclusive native tool isolation. Enrollment,
credential acquisition and proxy configuration remain outside this adapter.

## Configuration

MCP is disabled by default. Set `JOCASTA_MCP_RESOURCE_URL` or
`--mcp-resource-url=https://jocasta.example.com/mcp` before starting jocastad.
The existing `--oidc-issuer`, `--oidc-audience`, and `--oidc-jwks-url` configuration
is required. The REST audience remains unchanged; **MCP requires the exact
canonical resource URL in the signed JWT audience**, even when a JWT's audience
matches the REST client ID.

The resource URL must use canonical lowercase HTTPS scheme/host and the exact
path `/mcp`, without userinfo, query, fragment, escaped path, trailing slash,
trailing host dot, or an explicit default port. Nondefault ports are supported.
Disposable fixtures can explicitly add `--mcp-allow-loopback-http` for HTTP on
localhost or a loopback IP. This does not weaken issuer/JWKS TLS verification.

Expose the same `/mcp` path and
`/.well-known/oauth-protected-resource/mcp` through the authorized TLS ingress.
The latter is public RFC 9728 metadata naming the configured resource, issuer,
`jocasta.read` scope, and header-only bearer transport. Unauthenticated and
invalid/expired bearer requests receive HTTP 401 and a `WWW-Authenticate: Bearer`
challenge with `resource_metadata` and `scope`. A correctly signed, otherwise valid
JWT missing `jocasta.read` receives HTTP 403 with `error="insufficient_scope"`.
Expired tokens remain subject to the existing 30-second clock tolerance.
Malformed claims, missing/invalid project grants, wrong issuer/audience/signature,
and dotted-token verification failures receive 401, without operator fallback.

Opaque static operator credentials are REST-only. MCP requires the existing
bounded RS256 JWT contract: issuer, resource audience, subject, issued/expiry
times, at most 13 minutes lifetime, `jocasta.read`, and explicit
`jocasta_projects`. Every request revalidates the bearer; the SDK TokenInfo carries
that request's verified principal and grants, which each tool reapplies with
`store.WithReadAccess`. UserID alone never grants access. Only public JWKS material
is cached; authorization is not cached by subject or session.

An absent Origin header is allowed for machine clients. A supplied Origin must
exactly equal the configured resource's scheme and authority; empty, null,
multiple, and other origins are rejected with 403. Forwarded headers and Host do
not select a trusted origin. The SDK's default localhost DNS rebinding protection
also remains enabled. Metadata is public and uses the SDK's discovery CORS support;
this milestone does not add browser cross-origin tool access.

For a loopback backend behind TLS ingress, preserve the public Origin but rewrite
Host to the loopback upstream authority, or use an appropriately protected
non-loopback backend address. Forwarding the public Host unchanged to a loopback
listener triggers the SDK's DNS rebinding protection and returns 403. Keep that
protection enabled; the configured public resource URL still determines audience,
metadata and allowed Origin. Qualify the chosen ingress behavior before deployment.

## Tool contract

Tools use SDK-generated object input schemas with the following fields. Read fields are optional at the schema level; write fields and the semantic requirements below still apply. Unknown properties and wrong JSON types are rejected by SDK validation.

| Tool | Arguments | Successful structured result |
| --- | --- | --- |
| `get` | `path:string` or `id:string`; `revision:integer` positive when supplied; `lines:string` inclusive `start:end` | Existing `api.Receipt`, including exact document bytes, alias, range and hash fields |
| `list` | `prefix,project,kind,q,mode,cursor:string`; `limit:integer` positive when supplied; `include_archived:boolean` | Existing `api.Page` |
| `search` | Same as list; nonempty `q` required; `mode` retains store search semantics | Existing `api.Page`, including ranking/completeness |
| `history` | `path:string` or `id:string`; positive `limit:integer`; `cursor:string` | Existing `api.Page` of visible revisions |

Omitted revision means current; omitted limit uses the existing store default and
maximum. Store normalization, exact reference selection, aliases, project filtering,
historical visibility, archive behavior, and cursor validation are shared with
REST. Cursors bind the principal, grants, filters and visible corpus: a different
grant set fails validation; visible changes expire the cursor, while hidden
changes do not. Read receipts retain `jocasta/v1`; there is no new REST wire version.
Tool execution failures use SDK `isError` content containing the existing store
error code/message; successful responses provide structured content and JSON text.

With operator configuration absent, no mutating tool is available. Each authenticated request selects its catalog: limited or operator read-only credentials see `get`, `list`, `search`, `history`; operators with write scope also see `mutate`, `edit`. Catalog selection, write-handler checks and storage authorization all enforce the current request's grants. When operator trust is configured, SDK receiving middleware converts calls to learned document-write names on the read catalog into structured `forbidden` tool errors. They do not become HTTP authentication retries or close a valid read session.
Reads retain Jocasta's existing retention activity updates, without creating or
changing document revisions. No token is passed to another service; only the
pinned issuer's JWKS endpoint is contacted.

## Bounds and fixture verification

The transport is stateless and returns JSON responses. Each HTTP request is one
JSON-RPC object; batches are rejected. The read-only body limit is 64 KiB, including chunked bodies. Write-capable requests use the same bounded envelope as REST: `6*MaxDocumentBytes + 65536`, allowing JSON escaping of a 16-MiB document. The shared two-slot REST write limiter is acquired before reading any write-capable MCP POST body and released on every outcome. Both the pre-read and SDK apply the limit. Even read methods from a write-capable principal reserve this slot because allocation must be bounded before parsing tool arguments. Invalid UTF-8, unpaired Unicode escapes, malformed JSON and concatenated
objects are rejected. Shared admission remains eight concurrent requests,
600 requests per actor/minute, at most 4096 active rate entries, 8192 query bytes,
and a 30-second request deadline. Each tool also installs a 30-second store
deadline. SDK request cancellation propagation is enabled. Stateless GET/DELETE
streams and other unsupported methods return 405.

The loopback fixture uses the actual SDK HTTP client, a synthetic RS256/JWKS
issuer, and a disposable SQLite store. It covers allowed A/denied B via path,
UUID, aliases, history, list/search and cursors; narrower same-subject credentials
on the same client with a supplied old session ID; exact/range receipts; rejected
mutation with unchanged revision/content; invalid, expired, missing and
wrong-resource bearers; scope challenges; public metadata; Origin, body, malformed
message, capacity and rate checks. Each fixture sets and asserts a private scratch
HOME before database/token-file writes.

Run all checks on agent-a from the source worktree:

```sh
chaldea exec --host agent-a -- sh -eu -c '
  export HOME="$PWD/.cache/jocasta-http-mcp-builder/home"
  export TMPDIR="$HOME/tmp"
  test "$HOME" = "$PWD/.cache/jocasta-http-mcp-builder/home"
  mkdir -p "$TMPDIR"
  export HUYANG_FRICTION=0
  make check test cross
'
```

No Git-dependent test selection is needed for these make targets.

## Operational qualification prerequisites

The integration owner owns independent final reviews and integration. Before any
authorized disposable deployment, provide a local SQLite volume, restricted TLS
ingress preserving the two paths, a canonical resource URL, exact trusted issuer
and pinned JWKS URL, and a dedicated reporter grant for one allowed project with
a denied sibling. Confirm a newly issued test token's sanitized audience/resource,
scope, lifetime and project claims against this policy. A client-ID-only JWT must
not be accepted merely to make the test pass.

The disposable qualification above passed real Authentik resource audiences,
private credential acquisition and the selected official T3/native Claude and
Codex connections. Renewal during those composed runs, reboot recovery and
persistent host convergence remain separate acceptance checks. OpenCode is
deferred. The server's protocol, bearer verification, resource audience and
project ACLs are provider-independent. Compatibility must not widen audiences,
grants or operator access; a client's source capability alone proves no installed
runtime or exclusive tool boundary.

Protocol references:
[MCP authorization 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
and [the pinned SDK auth middleware](https://github.com/modelcontextprotocol/go-sdk/blob/bc72835f62eb94d0fb484439f886b6885b075f36/auth/auth.go).

## Optional operator OAuth

Configure all three settings together, before enabling MCP:

| Environment | Flag | Value |
| --- | --- | --- |
| `JOCASTA_OPERATOR_OIDC_ISSUER` | `--operator-oidc-issuer` | Exact HTTPS issuer, distinct from limited issuer |
| `JOCASTA_OPERATOR_OIDC_JWKS_URL` | `--operator-oidc-jwks-url` | Explicit pinned HTTPS JWKS endpoint |
| `JOCASTA_OPERATOR_OIDC_CLIENT_ID` | `--operator-oidc-client-id` | Fixed public OAuth client ID |

Defaults are empty. The operator audience is exactly one canonical MCP resource audience,
never a separate configurable audience or a combined audience set. A bounded unverified issuer selects one
of the two pinned verifiers; signature, issuer, audience, subject, time and
grants are then verified. Unknown issuers cause no key fetch. JWT-selected key
URLs are ignored. Verification failure never falls through to opaque tokens.

Recognized JWT claim names are exact and case-sensitive in both issuer selection
and verified decoding. Alternate-case aliases of registered, scope, authorized-party
or Jocasta grant fields are refused, including when a canonical field is also
present. Unrelated provider claims retain their ignored semantics.

The operator issuer requires boolean `jocasta_operator:true`, no
`jocasta_projects` field at all, and `azp` exactly equal to the configured
client ID. Null/false/string operator claims, combined project/operator grants,
and wrong client IDs are refused. `jocasta.read` authorizes all current and
future projects; `jocasta.write` additionally authorizes document mutations.
Write without read is invalid. The limited issuer refuses every operator field
and write scope, while retaining its existing project grant grammar.

Operator JWTs are accepted only at `/mcp`. Limited JWT REST reads and static
REST bearers keep their existing behavior. No JWT authorizes REST writes.
Document write permission does not authorize backup, reindex or inactivity
maintenance. Verified actor identity is installed in immutable storage context
and cannot be supplied as a tool argument. Cursors bind actor, limited/all
mode, sorted projects and write capability; token renewal with the same
effective permissions preserves them. Operator searches retain full-corpus
ranking, and operator cursors expire on global document changes. Limited
searches/cursors retain their project-filtered behavior.

When operator trust is configured, protected-resource metadata lists that issuer
first, followed by the limited issuer, and advertises read/write scopes.
Otherwise existing metadata remains unchanged. The separate public Authentik
application uses `jocasta-laptop-interactive`, its dedicated issuer ending
`/application/o/jocasta-laptop-interactive/`, and that issuer's `jwks/`.
Issuer enrollment, exact redirect/PKCE policies, native login, refresh rotation
and revocation acceptance belong to deployment. Source tests do not qualify
real native OAuth or grant activation authority.

## Document mutations and exact edits

`mutate` accepts the existing flat `api.Mutation` JSON fields plus required
`idempotency_key`. Operations are `create`, `put`, `move`, `archive`,
`unarchive`, `restore`. Existing content presence, CAS, alias, history, audit
actor/message, transaction and content-free receipt semantics are shared with
REST. Unknown fields, including `actor`, are refused.

`edit` accepts exactly one `path` or `id`, positive `if_revision`,
nonempty `old`, required `new` which may be empty, `idempotency_key`, and
optional `section` and `message`. It replaces exactly one byte match within
the document or selected section. Zero, multiple and overlapping matches refuse
with `edit_match`. No fuzzy matching, regex, inferred search target or conflict
retry occurs. Untouched UTF-8, CRLF and final-newline bytes remain exact.

The original edit request has a domain-separated digest in the existing receipt
table. Actor/key and digest are checked inside the write transaction before
current revision, section or match checks. An exact retry returns the original
receipt even after later writes or moves. Different input under the same
actor/key returns `idempotency_conflict`. Mutations retain legacy digests.
Edit is MCP-only; there is no new REST/CLI operation or receipt store.

## Outline, sections and search excerpts

Plain `get`, `list`, `search`, `history` retain their existing results.
Optional `get` arguments choose one of:

- Existing `lines:"start:end"`.
- `view:"outline"`, optional `levels:[1,2,3,4,5,6]` and
  `outline_limit:1..200`. Defaults are all levels and at most 200 entries.
- `section:{"heading":"Exact source label","heading_line":N}`.
  The line is optional and disambiguates duplicate labels. Missing or ambiguous
  selectors refuse with `section_not_found` or `ambiguous_section`; ambiguity
  errors name at most 20 candidate heading lines. Both supplied fields must match.

`revision` pins any get mode. New results carry document ID/path, revision,
full content hash and total lines. Outline entries name heading and section
line ranges. Sections include the heading and end before the next heading of
equal/lower level or EOF. New views stay within a 64-KiB JSON budget, report
`complete`/`truncated`, and direct remaining reads to `next_lines` on the
same ID/revision/hash. Section results additionally report exact zero-based
half-open byte ranges; an end line may be partial. Oversized selectors refuse
with a pinned-line-read recommendation. The content budget reserves worst-case
JSON escaping space rather than copying an unbounded string and trimming later.

`search` adds `include_excerpts:true` and optional `excerpt_bytes:1..2048`.
Enriched search defaults to 20 results and refuses an explicit limit above 20.
Its existing page fields/ranking/filter/cursor are unchanged; an `excerpts`
array adds exact ID/revision/hash, bytes, zero-based half-open byte ranges,
inclusive line ranges, nearest preceding heading and completeness. The existing
page's content preview remains unchanged for compatibility. Each new excerpt
is fetched through authorized get at the returned immutable revision. A pinned
revision/hash mismatch refuses instead of attaching current bytes.

Placement uses the earliest exact case-sensitive whitespace-delimited query
term in the body. Metadata-only or differently cased hits may have no exact
literal match, reported as `reason:"no_exact_literal_match"`. A bounded
excerpt begins on the matching line or at the match in a very long line.
`truncated` means it does not contain the entire body. A no-match excerpt for a
nonempty body is incomplete/truncated and points to pinned line reads starting
at line 1. An empty body is complete even when there is no literal match.
Results above the
64-KiB total budget refuse with a smaller-limit/pinned-read recommendation;
the adapter never drops results while advancing the original cursor.
Every view and excerpt resolves ACLs before locator/content work, including
aliases, current paths and historical paths.

### Locator grammar

The shared locator indexes source bytes without normalization or rendering.
It recognizes top-level ATX headings with zero to three leading spaces, one
to six hashes, and whitespace or EOL after the hashes. Optional trailing hashes
require preceding whitespace. Single-line Setext headings require an eligible
immediately preceding top-level nonblank text line. Labels remove structural
markers and trim whitespace while preserving inline Markdown, case and Unicode
spelling. They are not rendered text or slugs.

Backtick and tilde fences require at least three delimiters; a closing fence
uses the same delimiter with at least the opening length and only trailing
whitespace. Backtick info cannot contain backticks. An unclosed fence consumes
the remainder. Four-space/tab indented code, quote-prefixed lines, list item
lines and their indented continuations are excluded. Leading tabs advance to
four-column stops when determining continuation indentation, without changing
source byte offsets. A nonblank line at or
before the list item's indentation ends that bounded list context. HTML heading
rendering, nested container parsing and full CommonMark are outside this grammar.
The locator refuses more than 10,000 headings rather than silently indexing a
partial document. It does not allocate a record for every source line.
