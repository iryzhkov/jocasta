# Standalone Authentik read-only canary

This tests REST authorization on disposable Jocasta data. Authentik issuance,
entitlement mappings, TTLs, T3 launch restrictions and process isolation remain
unqualified. A trusted operator agent can still read its operator credentials.
There is no deployment change, Chaldea product integration, MCP SDK or runtime
Authentik admin credential.

## Fixed source contract

OIDC is off by default; static operator tokens retain full authority. Configure
all three settings, or matching flags. Partial configuration fails startup.

| Environment | Flag | Meaning |
| --- | --- | --- |
| JOCASTA_OIDC_ISSUER | --oidc-issuer | Exact HTTPS iss, including trailing slash |
| JOCASTA_OIDC_AUDIENCE | --oidc-audience | Required aud member |
| JOCASTA_OIDC_JWKS_URL | --oidc-jwks-url | Explicit HTTPS public key endpoint |

Issuer/key URLs accept no credentials, query or fragment. There is no discovery,
redirect, token-supplied key URL or insecure TLS option. System trust roots apply.
RS256 is the only allowed algorithm. Required signed claims:

- iss matches exactly; aud contains the configured audience.
- sub is nonempty, at most 256 bytes. Identity is issuer plus sub, not username,
  email, group or requested project.
- exp and iat are required; exp > iat and exp - iat <= 780 seconds. Optional nbf
  is validated and must precede exp. Maximum clock leeway is 30 seconds.
  Provision 300-second access tokens; effective expiry may extend 30 seconds.
- scope is a string containing the exact scope jocasta.read. Extra scopes grant
  no writes.
- jocasta_projects is a nonempty JSON string array of at most 16 exact NFC
  project segments, each <=128 bytes. No slash, wildcard, controls, traversal or
  reserved leading underscore. Enroll exactly ["canaryA"] for the first account.

Example payload, with real issuer, subject and times substituted:

```json
{"iss":"https://auth.example/application/o/jocasta-canary/","aud":"jocasta-canary","sub":"machine-subject","iat":1792400000,"nbf":1792400000,"exp":1792400300,"scope":"jocasta.read","jocasta_projects":["canaryA"]}
```

Grants belong to each token, including tokens with the same sub. With OIDC
enabled, dotted bearers are reserved for JWTs and failed verification never falls
back to operator authority, even with a matching legacy entry. Legacy-only mode
preserves existing opaque tokens, including dots. Rotate dotted opaque operator
tokens before enabling mixed OIDC/static mode. Explicit --tokens-file="" enables
OIDC-only use; that is the required disposable-canary configuration.

JWTs may only GET /v1/get, /v1/list, /v1/search and /v1/history. Health is public.
Other routes/methods return 403 before body processing or writes. Restricted
store contexts reject mutations, maintenance and receipt replays. Authorized
get still records read activity transactionally after authorization.

Direct reads require access to the supplied alias, current target and requested
historical snapshot. Denied/missing references share the same 404. History filters
each revision's path before pagination. Archived data, ranges, CLI exists and
CLI diff follow the same checks. A formerly allowed document now in a denied
project cannot be read through an old alias or historical ID reference.

Project SQL predicates precede candidates, snippets, counts and pagination.
Scoped search uses the existing literal scan, reported as ranking=literal and
ranking_reason=project_scope, to avoid global FTS statistics. Only allowed aliases
contribute matches. Existing 500-candidate/32-MiB scan limits and incomplete-page
indicators apply; empty search remains an input error. No fuzzy/semantic ranking
is offered to scoped readers. Operator queries are unchanged. Restricted cursors
bind to issuer/sub, effective projects and filters. Page state observes only
allowed documents; service restarts invalidate restricted cursors.

JWKS fetches allow <=64 KiB and <=16 public RSA keys of 2048..4096 bits, with a
three-second timeout, one-minute cache, one fetch in flight and five-second retry
floor. Expired keys fail closed on refresh failure. Rotate with a new kid; reused
kid changes can take one minute. Key removal does not revoke cached keys at once.
Signed grants remain effective until token expiry; there is no introspection.

## Disposable exercise

Run source checks from the worktree on your configured build host, replacing
`BUILD_HOST` with its name:

```sh
chaldea exec --host BUILD_HOST -- sh -ceu '
  export HOME="$PWD/.cache/canary-home"
  test "$HOME" = "$PWD/.cache/canary-home"
  export TMPDIR="$HOME/tmp"
  mkdir -p "$TMPDIR"
  export GOCACHE="$HOME/go-build" GOPATH="$HOME/go" HUYANG_FRICTION=0
  make check test cross
'
```

Tests use ephemeral TLS JWKS/signing fixtures and SQLite data, without contacting
installed Authentik. For a later approved local live exercise, use reviewed
binaries, a private local directory and separate operator/client credentials:

```sh
umask 077
export CANARY_DIR="$HOME/.cache/jocasta-authentik-live-canary"
mkdir -p "$CANARY_DIR"
python3 - <<'PY'
import json, os, pathlib, secrets
root = pathlib.Path(os.environ["CANARY_DIR"])
token = secrets.token_hex(32)
(root / "seed-token").write_text(token)
(root / "seed-tokens.json").write_text(json.dumps({"seed-operator": token}))
for name, filename in [("seed", "seed-token"), ("read", "read-token")]:
    (root / (name + "-client.json")).write_text(json.dumps({
        "url": "http://127.0.0.1:18081", "allow_loopback_http": True,
        "token_file": str(root / filename)}))
PY
jocastad serve --db "$CANARY_DIR/disposable.db" --listen 127.0.0.1:18081 \
  --tokens-file "$CANARY_DIR/seed-tokens.json"
```

In another operator shell with the same CANARY_DIR, seed two fixtures:

```sh
printf 'allowed-marker\n' | jocasta --config "$CANARY_DIR/seed-client.json" put canaryA/allowed --create --file -
printf 'denied-marker\n' | jocasta --config "$CANARY_DIR/seed-client.json" put canaryB/denied --create --file -
```

Stop the seed instance. Set the three approved public OIDC values and restart:

```sh
jocastad serve --db "$CANARY_DIR/disposable.db" --listen 127.0.0.1:18081 \
  --tokens-file="" --oidc-issuer "$CANARY_ISSUER" \
  --oidc-audience "$CANARY_AUDIENCE" --oidc-jwks-url "$CANARY_JWKS_URL"
```

This backend is loopback-only. Remote clients require the existing TLS ingress;
deployment is outside this task. Issuer/JWKS HTTPS verification always applies.

A separately approved private credential helper must acquire an access JWT and
atomically write $CANARY_DIR/read-token at mode 0600 under the 0700 directory.
No helper implementation or enrollment is included here. Store its machine
credential privately; no bearer/secret argv, shell tracing, environment dumps,
logs or evidence. The helper needs no admin API permission. Never give the
limited client the seed operator credential. Then exercise the existing CLI:

```sh
jocasta --config "$CANARY_DIR/read-client.json" get canaryA/allowed
jocasta --config "$CANARY_DIR/read-client.json" exists canaryB/denied --json
jocasta --config "$CANARY_DIR/read-client.json" list --json
jocasta --config "$CANARY_DIR/read-client.json" search denied-marker --json
printf 'blocked\n' | jocasta --config "$CANARY_DIR/read-client.json" put canaryA/allowed --if-revision 1 --file -
```

Expect exact allowed content, exists false/exit 2, only canaryA listed, no denied
marker and write HTTP 403. Complete incomplete search pages before declaring
absence. The CLI loads its token file per invocation and does no OAuth refresh.
Refresh through the private helper, then invoke again. Retain sanitized outcomes,
status codes and lifetime numbers only.

## Live enrollment packet, pending

A separate provisioning identity must approve one ordinary nonadmin machine
account and one dedicated OAuth2/OIDC provider/application. Do not reuse existing
human SSO applications. The ordinary account must have no admin/superuser groups, global write
permissions, token/key viewing rights or ability to edit its entitlement mapping.
Provisioning credentials and signing keys stay outside both runtime and helper.

Before enrollment against the installed Authentik version, record the exact issuer, audience,
public JWKS URL, RS256 kid and machine issuance flow. Provision fixed mappings
for scope jocasta.read and jocasta_projects ["canaryA"]. Caller-requested
scope/project values must never expand signed bounds. Verify access JWTs rather
than browser ID tokens. Confirm claim types and whether the machine flow uses a
client-associated sub; establish its stability before treating it as a user ID.
Human SSO success does not qualify machine tokens.

Live acceptance still needs real five-minute issuance, wrong audience/issuer,
future iat/nbf, expiry with skew, renewal, entitlement removal, attempted grant
expansion and JWKS rotation checks. Exercise both projects through paths, IDs,
aliases, historical revisions, ranges, exists/diff, archived data, all search
modes, cursors and every mutation. Verify denied requests leave no document,
revision, receipt or activity writes. Confirm separately that the machine cannot
administer Authentik, inspect signing keys or view others' tokens. Stop at
disposable data until these prerequisites pass. No installed hosts were changed.
