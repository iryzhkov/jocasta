# Opt-in HTTP MCP reader

Jocastad can expose four read-only tools at `/mcp` using the official
[Go SDK v1.7.0](https://github.com/modelcontextprotocol/go-sdk/tree/bc72835f62eb94d0fb484439f886b6885b075f36).
The annotated tag `25cb00203c6b693780f602ab4041c06f7f4b9570` resolves to
`bc72835f62eb94d0fb484439f886b6885b075f36`. The dependency and its module checksums
are pinned in go.mod/go.sum; its Go requirement is 1.25.0, matching Jocasta.

This is a source milestone with synthetic issuer proof. Actual Authentik resource
audience behavior, installed client renewal, and T3 third-party MCP wiring remain
unqualified. No enrollment, token acquisition, proxy, refresher, or client code is
part of this adapter.

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

Tools use SDK-generated object input schemas with the following fields. All
fields are optional at the schema level; the semantic requirements below still
apply. Unknown properties and wrong JSON types are rejected by SDK validation.

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

No mutating tool is registered. Unknown `mutate` calls are rejected by SDK dispatch.
Reads retain Jocasta's existing retention activity updates, without creating or
changing document revisions. No token is passed to another service; only the
pinned issuer's JWKS endpoint is contacted.

## Bounds and fixture verification

The transport is stateless and returns JSON responses. Each HTTP request is one
JSON-RPC object; batches are rejected. The body limit is 64 KiB, including chunked
bodies. Invalid UTF-8, unpaired Unicode escapes, malformed JSON and concatenated
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

Actual Authentik acceptance of this resource audience is pending. Client helper
configuration, credential retrieval/issuance, installed runtime behavior after a
401, and T3's arbitrary external MCP configuration are separate qualification
steps owned by the parent. The server's protocol, bearer verification, resource audience and project ACLs are
provider-independent. Codex, Claude Code and OpenCode each require separate
connection, private token acquisition and renewal qualification; all three are
pending. Upstream Codex helper source capability is a candidate only and does not
select an exclusive client or prove any installed runtime or T3 seam. Compatibility
must not widen audiences, grants or operator access. This milestone makes no
deployed T3, login, renewal, host-convergence, or hosted-CI claim.

Protocol references:
[MCP authorization 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
and [the pinned SDK auth middleware](https://github.com/modelcontextprotocol/go-sdk/blob/bc72835f62eb94d0fb484439f886b6885b075f36/auth/auth.go).
