package server

import (
	"testing"

	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
)

func TestManagedHostSubjectCoexistence(t *testing.T) {
	f, o := newOperatorFixture(t)
	// Keep the managed issuer/client. Only JWKS uses the fixture HTTPS server.
	o.verifier.cfg.Issuer = "https://auth.ryzhkov.dev/application/o/jocasta-managed-interactive/"
	o.verifier.operatorClientID = "jocasta-managed-interactive"
	actors := map[string]bool{}
	for _, host := range []string{"laptop", "omarchy-pc", "agent-a", "agent-b"} {
		for _, username := range []string{"webmcp-interactive-" + host, "chaldea-" + host} {
			t.Run(username, func(t *testing.T) {
				c := operatorClaims(f, o, true)
				// Fixture hashes stand in for verified Authentik sub values.
				subject := store.Hash("synthetic:" + username)
				c["sub"], c["webmcp_identity"] = subject, username
				c["azp"], c["scope"] = "jocasta-managed-interactive", "jocasta.read jocasta.write"
				bearer := &mcpBearerTransport{token: signed(t, o.key, c)}
				session := f.client(t, bearer)
				var created, got api.Receipt
				path := "coexistence/" + username
				mcpCall(t, session, "mutate", map[string]any{
					"operation": "create", "path": path, "content": username,
					"idempotency_key": "create-" + username,
				}, &created)
				mcpCall(t, session, "get", map[string]any{"path": path}, &got)
				actor := "oidc:" + store.Hash(o.verifier.cfg.Issuer+"\x00"+subject)
				if got.Document.Content != username || created.Document.Actor != actor || actors[actor] {
					t.Fatal("operator access or attribution changed", created, got)
				}
				actors[actor] = true
				mcpCall(t, session, "mutate", map[string]any{
					"operation": "create", "path": "other/" + username, "content": "denied",
					"idempotency_key": "other-" + username,
				}, &got)
				// The same signed host subject gains no write authority from its name.
				c["scope"] = "jocasta.read"
				bearer.set(signed(t, o.key, c))
				mcpError(t, session, "mutate", map[string]any{
					"operation": "archive", "id": created.Document.ID,
					"if_revision": 1, "idempotency_key": "readonly-" + username,
				}, "forbidden")
				limited := f.oidc.claims("coexistence")
				limited["sub"], limited["aud"] = subject, f.server.mcpResource
				bearer.set(signed(t, f.oidc.key, limited))
				mcpCall(t, session, "get", map[string]any{"path": path}, &got)
				mcpError(t, session, "mutate", map[string]any{
					"operation": "archive", "id": created.Document.ID,
					"if_revision": 1, "idempotency_key": "limited-" + username,
				}, "forbidden")
				mcpError(t, session, "get", map[string]any{"path": "other/" + username}, "not_found")
				mcpCall(t, session, "get", map[string]any{"path": path}, &got)
				if got.Document.Revision != 1 || got.Document.Archived {
					t.Fatal("denied write changed document", got)
				}
			})
		}
	}
}
