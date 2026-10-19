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

### Private helper

`scripts/authentik_canary_client.py` is a Python 3 stdlib launcher for this
operator-only canary. Enrollment and any live invocation still require the
protected packet below. It is not a production broker, shared auth framework,
MCP client or process isolation boundary. A trusted operator can read its own
files and change its own programs/configuration.

The issuance contract is Authentik 2026.8.4, source commit
`410f46e5cf18f97a1f0056b30984a145242df55d`. Its
`token/client_credentials.py` accepts form `grant_type=client_credentials`,
`client_id`, `client_secret=<dedicated provider secret>` and `scope=jocasta.read`.
No username or app password is sent. The provider-secret path creates or updates
`ak-<provider.name>-client_credentials`; precreate that exact ordinary service
account without roles, groups, permissions or login credentials, then bind only
that account to the dedicated application before connecting its provider.
`token.py:create_client_credentials_response` returns access-token metadata
without a refresh token. The helper makes no admin API call. Disable the
provider grant or detach the application to stop issuance; account disablement
alone is not qualified for this generated-account path.

After separate enrollment approval, provision a protected JSON file with exactly
these fields. All paths must be absolute and normalized, without symlink
components. Example public settings and credential-file reference only:

```json
{
  "expected_issuer": "https://auth.example/application/o/jocasta-canary/",
  "client_id": "jocasta-canary",
  "token_url": "https://auth.example/application/o/token/",
  "client_secret_file": "/home/operator/.cache/jocasta-authentik-live-canary/client-secret",
  "private_dir": "/home/operator/.cache/jocasta-authentik-live-canary",
  "jocasta_binary": "/home/operator/reviewed-bin/jocasta",
  "jocasta_url": "http://127.0.0.1:18081"
}
```

Set the existing private directory to 0700. The helper config and client-secret file
must be owned by the invoking user, regular, single-link files at 0400 or 0600.
The client-secret file contains only the dedicated provider secret, optionally followed by one
newline. Never put a literal secret in JSON, argv, environment, logs or
evidence. Provision it through the separately approved private enrollment
procedure. Never copy seed/operator/static tokens into the helper.

Pin the approved issuer and client ID in this config and configure Jocasta's
server issuer/audience to those same values. `expected_issuer` checks only the
helper token URL origin; the server enforces the complete signed issuer. The token URL must be exact HTTPS,
on the issuer's origin, without URL credentials, query or fragment. The helper
uses system TLS trust, no proxies, redirects, discovery or retries, and a
10-second total request deadline with a 32-KiB response limit. It requires a
nonempty bearer access token of at most 16 KiB, integer `expires_in` from 1 to
300 and exactly `scope=jocasta.read`. It ignores ID/refresh-token fields.

Each invocation acquires a fresh access token. This provides automatic renewal
without a refresh token or cache protocol. Acquisition failure prevents CLI
execution even when an old token file remains. The helper treats tokens as
opaque. It does not verify JWT signatures or claims, and response metadata is
not proof of token issuer/audience/expiry. Jocasta's verifier must enforce the
signed issuer, audience, expiry and project bounds described above.

The helper atomically replaces `private_dir/read-token` at 0600, then generates
`private_dir/read-client.json` at 0600 with only the configured loopback URL,
`allow_loopback_http:true` and that exact token-file path. Existing outputs must
also be owner-only regular single-link files at 0600; symlinks and unsafe types
or modes are refused. Jocasta's URL must be a literal `http://127.0.0.1:PORT`
or `http://[::1]:PORT` origin. No CLI URL, path or credential override
is accepted. Launch Python with the scrubbed environment below to avoid inherited
CA and Python environment overrides. The reviewed binary must be an absolute regular executable without
symlink components or group/world write permission. Verify its approved SHA-256
before the live exercise.

The launcher execs only that binary with the generated `--config` path, a small
public environment and the supplied command. HOME/TMPDIR point to the private
directory for local CLI state; that is not process isolation. Do not run
concurrent same-user modifications during the exercise. Read commands are
`get`, `exists`, `ls`, `search`, `history` and `diff`, plus `put` for the
write-denial exercise. The wrapper accepts a fixed subset of each command's
options from `internal/cli/cli.go` and `help.go`. Use long options after the
verb. Config/credential/URL overrides, unknown or duplicate options, local
output options and arbitrary programs are refused; `put` requires `--file -`.
Use `ls`, the actual CLI verb, for listing.

After approval, from this source tree with the private config path substituted:

```sh
canary_client() {
  env -i HOME="$CANARY_DIR" TMPDIR="$CANARY_DIR" PATH=/usr/bin:/bin \
    /usr/bin/python3 -I scripts/authentik_canary_client.py "$CANARY_DIR/helper.json" "$@"
}
canary_client get canaryA/allowed
canary_client exists canaryB/denied --json
canary_client ls --json
canary_client search denied-marker --json
printf 'blocked\n' | canary_client put canaryA/allowed --if-revision 1 --file -
```

Expect exact allowed content, exists false/exit 2, only canaryA listed, no denied
marker and write HTTP 403. Complete incomplete search pages before declaring
absence. Successful exec preserves the CLI's exit code. Helper failures print
one fixed diagnostic and exit 1 without exception text or response bodies.
Retain sanitized outcomes, status codes and lifetime numbers only.

Run the synthetic helper suite on agent-a. This asserts the scratch HOME before
creating fixtures; no fixture uses real credentials or a real login:

```sh
chaldea exec --host agent-a -- sh -ceu '
  exec env HOME="${PWD%/}/.cache/authentik-helper-home" \
    TMPDIR="${PWD%/}/.cache/authentik-helper-home/tmp" \
    PYTHONDONTWRITEBYTECODE=1 sh -ceu '\''
      test "$HOME" = "${PWD%/}/.cache/authentik-helper-home"
      test "$TMPDIR" = "$HOME/tmp"
      umask 077
      mkdir -p "$TMPDIR"
      python3 -m unittest discover -s scripts -p test_authentik_canary_client.py -v
    '\''
'
```

## Live enrollment packet, pending

The separately approved provisioning procedure creates one ordinary nonadmin machine
account and one dedicated OAuth2/OIDC provider/application. Use only the
provider-specific client-credentials grant. Keep the exact-account application
binding until the provider/application has been disconnected during cleanup. Do not reuse existing
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

First live acceptance needs real five-minute issuance, allowed/denied project
calls, write denial, wrong verifier audience/issuer, fresh reacquisition after
five minutes and issuance refusal after disabling the provider grant. Signed
claim inspection, old-token replay, future iat/nbf, requested-scope expansion,
key rotation and Authentik admin-denial probes require separate reviewed tools
and remain deferred. Exercise both projects through paths, IDs,
aliases, historical revisions, ranges, exists/diff, archived data, all search
modes, cursors and every mutation. Verify denied requests leave no document,
revision, receipt or activity writes. Administrative rights must be absent by enrollment configuration; live API
negative probes remain unqualified until reviewed tooling exists. Stop at
disposable data until these prerequisites pass. No installed hosts were changed.
