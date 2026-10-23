package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const mcpBodyLimit = 64 << 10

// The SDK emits the metadata/scope challenge. Add the RFC 6750 error code so
// clients can distinguish a real scope refusal from a renewable expired token.
type mcpAuthResponse struct{ http.ResponseWriter }

func (w mcpAuthResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w mcpAuthResponse) WriteHeader(status int) {
	if status == http.StatusForbidden && w.Header().Get("WWW-Authenticate") != "" {
		w.Header().Set("WWW-Authenticate", w.Header().Get("WWW-Authenticate")+`, error="insufficient_scope"`)
	}
	w.ResponseWriter.WriteHeader(status)
}

// EnableMCP must be called before serving requests. Only the public /mcp resource
// is supported; ingress must preserve that path. No request header defines trust.
func (s *Server) EnableMCP(resource string, allowLoopbackHTTP bool) error {
	u, err := url.Parse(resource)
	if err != nil || len(resource) > 2048 || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || u.Path != "/mcp" || u.RawPath != "" ||
		u.Host != strings.ToLower(u.Host) || strings.HasSuffix(u.Hostname(), ".") ||
		u.String() != resource || strings.HasSuffix(u.Host, ":") {
		return errors.New("MCP resource must be a canonical public URL ending in /mcp")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port ||
			(u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
			return errors.New("MCP resource needs a canonical port")
		}
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback && allowLoopbackHTTP) {
		return errors.New("MCP resource requires HTTPS (explicit loopback HTTP is fixture-only)")
	}
	if s.cfg.OIDC == nil {
		return errors.New("MCP requires configured OIDC verification")
	}
	s.mcpResource = resource
	s.mcpOrigin = u.Scheme + "://" + u.Host
	s.mcpMetadata = auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource: resource, AuthorizationServers: []string{s.cfg.OIDC.cfg.Issuer},
		ScopesSupported: []string{"jocasta.read"}, BearerMethodsSupported: []string{"header"},
		ResourceName: "Jocasta read-only documents",
	})
	srv := mcp.NewServer(&mcp.Implementation{Name: "jocasta", Version: api.Version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}})
	addJocastaReadTool(srv, "get", "Get an exact document or revision; optional line range preserves receipt hashes.",
		func(ctx context.Context, a mcpGetArgs) (any, error) {
			revision := int64(0)
			if a.Revision != nil {
				if *a.Revision < 1 {
					return nil, store.Invalid("revision must be positive when supplied")
				}
				revision = *a.Revision
			}
			r, err := s.s.Get(ctx, a.Path, a.ID, revision)
			if err == nil && a.Lines != "" {
				r, err = store.Range(r, a.Lines)
			}
			return r, err
		})
	addJocastaReadTool(srv, "list", "List authorized documents with Jocasta grant-bound cursors.",
		func(ctx context.Context, a mcpPageArgs) (any, error) {
			if a.Limit != nil && *a.Limit < 1 {
				return nil, store.Invalid("invalid limit")
			}
			return s.s.Page(ctx, a.filter(), false)
		})
	addJocastaReadTool(srv, "search", "Search authorized documents; q is required. Results retain ranking and completeness receipts.",
		func(ctx context.Context, a mcpPageArgs) (any, error) {
			if a.Limit != nil && *a.Limit < 1 {
				return nil, store.Invalid("invalid limit")
			}
			return s.s.Page(ctx, a.filter(), true)
		})
	addJocastaReadTool(srv, "history", "Read authorized revision history with Jocasta grant-bound cursors.",
		func(ctx context.Context, a mcpHistoryArgs) (any, error) {
			if a.Limit != nil && *a.Limit < 1 {
				return nil, store.Invalid("invalid limit")
			}
			limit := 0
			if a.Limit != nil {
				limit = *a.Limit
			}
			return s.s.History(ctx, a.Path, a.ID, limit, a.Cursor)
		})
	s.mcp = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
			MaxRequestBodyBytes: mcpBodyLimit, PropagateRequestCancellation: true})
	metadataURL := s.mcpOrigin + "/.well-known/oauth-protected-resource/mcp"
	s.mcpAuth = auth.RequireBearerToken(func(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		// Reuse strict header handling and the no-JWT-fallback rule. Opaque
		// operator bearers remain REST-only, even when configured alongside OIDC.
		p, err := s.authenticate(r)
		if err != nil || p.claims == nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: p.actor, Expiration: p.claims.ExpiresAt.Time,
			Scopes: strings.Fields(p.claims.Scope), Extra: map[string]any{"principal": p}}, nil
	}, &auth.RequireBearerTokenOptions{ResourceMetadataURL: metadataURL, Scopes: []string{"jocasta.read"},
		ClockSkew: tokenLeeway})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.TokenInfoFromContext(r.Context()).Extra["principal"].(principal)
		if !ok || p.claims == nil {
			http.Error(w, "invalid principal", http.StatusUnauthorized)
			return
		}
		s.serveAuthenticated(w, r, p)
	}))
	return nil
}

type mcpGetArgs struct {
	Path     string `json:"path,omitempty" jsonschema:"Exact document path; provide path or id."`
	ID       string `json:"id,omitempty" jsonschema:"Exact document UUID; provide path or id."`
	Revision *int64 `json:"revision,omitempty" jsonschema:"Positive revision; omitted means current."`
	Lines    string `json:"lines,omitempty" jsonschema:"Optional inclusive line range start:end."`
}
type mcpPageArgs struct {
	Prefix          string `json:"prefix,omitempty"`
	Project         string `json:"project,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Query           string `json:"q,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	Limit           *int   `json:"limit,omitempty"`
	IncludeArchived bool   `json:"include_archived,omitempty"`
}

func (a mcpPageArgs) filter() api.Filter {
	limit := 0
	if a.Limit != nil {
		limit = *a.Limit
	}
	return api.Filter{Prefix: a.Prefix, Project: a.Project, Kind: a.Kind, Query: a.Query,
		Mode: a.Mode, Cursor: a.Cursor, Limit: limit, IncludeArchived: a.IncludeArchived}
}

type mcpHistoryArgs struct {
	Path   string `json:"path,omitempty"`
	ID     string `json:"id,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// Reapply grants from this HTTP request's verified TokenInfo. UserID alone never
// authorizes a tool, and no session or subject cache retains broader grants.
func addJocastaReadTool[In any](srv *mcp.Server, name, description string, run func(context.Context, In) (any, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, any, error) {
			if req.Extra == nil || req.Extra.TokenInfo == nil {
				return nil, nil, errors.New("verified principal required")
			}
			p, ok := req.Extra.TokenInfo.Extra["principal"].(principal)
			if !ok || p.claims == nil {
				return nil, nil, errors.New("verified grants required")
			}
			ctx, err := store.WithReadAccess(ctx, p.actor, p.claims.Projects)
			if err != nil {
				return nil, nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := run(ctx, args)
			return nil, out, err
		})
}
