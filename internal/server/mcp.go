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

	"github.com/iryzhkov/jocasta/internal/markdown"
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
	if s.cfg.OIDC == nil || s.cfg.OIDC.operatorClientID != "" {
		return errors.New("MCP requires configured OIDC verification")
	}
	if s.cfg.OperatorOIDC != nil && (s.cfg.OperatorOIDC.operatorClientID == "" || s.cfg.OperatorOIDC.cfg.Issuer == s.cfg.OIDC.cfg.Issuer) {
		return errors.New("operator requires distinct explicitly configured verifier")
	}
	s.mcpResource = resource
	issuers := []string{s.cfg.OIDC.cfg.Issuer}
	scopes := []string{"jocasta.read"}
	name := "Jocasta read-only documents"
	if s.cfg.OperatorOIDC != nil {
		issuers = append([]string{s.cfg.OperatorOIDC.cfg.Issuer}, issuers...)
		scopes = append(scopes, "jocasta.write")
		name = "Jocasta documents"
	}
	s.mcpOrigin = u.Scheme + "://" + u.Host
	s.mcpMetadata = auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource: resource, AuthorizationServers: issuers,
		ScopesSupported: scopes, BearerMethodsSupported: []string{"header"},
		ResourceName: name,
	})
	readSrv := mcp.NewServer(&mcp.Implementation{Name: "jocasta", Version: api.Version}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}})
	if s.cfg.OperatorOIDC != nil {
		readSrv.AddReceivingMiddleware(refuseDocumentWrites)
	}
	fullSrv := mcp.NewServer(&mcp.Implementation{Name: "jocasta", Version: api.Version}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}})
	for _, srv := range []*mcp.Server{readSrv, fullSrv} {

		addJocastaReadTool(srv, "get", "Get an exact document or revision; optional line range preserves receipt hashes.",
			func(ctx context.Context, a mcpGetArgs) (any, error) {
				return s.getView(ctx, a)
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
				return s.searchView(ctx, a)
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
	}
	addJocastaWriteTool(fullSrv, "mutate", "Apply create, put, move, archive, unarchive or restore with CAS and idempotency.",
		func(ctx context.Context, actor string, a mcpMutationArgs) (any, error) {
			return s.s.Mutate(ctx, actor, a.IdempotencyKey, a.Mutation)
		})
	addJocastaWriteTool(fullSrv, "edit", "Replace unique exact old bytes, optionally in an exact section; requires CAS and idempotency.",
		func(ctx context.Context, actor string, a mcpEditArgs) (any, error) {
			return s.s.Edit(ctx, actor, a.IdempotencyKey, a.EditRequest)
		})
	readHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return readSrv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: mcpBodyLimit, PropagateRequestCancellation: true})
	writeHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fullSrv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: s.mcpWriteBodyLimit(), PropagateRequestCancellation: true})
	s.mcp = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		if info != nil {
			if p, ok := info.Extra["principal"].(principal); ok && p.canMutate() {
				writeHandler.ServeHTTP(w, r)
				return
			}
		}
		readHandler.ServeHTTP(w, r)
	})
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
	Path         string             `json:"path,omitempty" jsonschema:"Exact document path; provide path or id."`
	ID           string             `json:"id,omitempty" jsonschema:"Exact document UUID; provide path or id."`
	Revision     *int64             `json:"revision,omitempty" jsonschema:"Positive revision; omitted means current."`
	View         string             `json:"view,omitempty" jsonschema:"Optional outline; mutually exclusive with section and lines."`
	Section      *markdown.Selector `json:"section,omitempty"`
	Levels       []int              `json:"levels,omitempty" jsonschema:"Outline heading levels 1 through 6; default all."`
	OutlineLimit int                `json:"outline_limit,omitempty"`
	Lines        string             `json:"lines,omitempty" jsonschema:"Optional inclusive line range start:end."`
}
type mcpPageArgs struct {
	Prefix          string `json:"prefix,omitempty"`
	Project         string `json:"project,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Query           string `json:"q,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	Limit           *int   `json:"limit,omitempty"`
	IncludeExcerpts bool   `json:"include_excerpts,omitempty"`
	ExcerptBytes    int    `json:"excerpt_bytes,omitempty"`
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
			if !ok || p.claims == nil || !hasScope(p.claims.Scope, "jocasta.read") {
				return nil, nil, errors.New("verified grants required")
			}
			ctx, err := p.accessContext(ctx)
			if err != nil {
				return nil, nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := run(ctx, args)
			return nil, out, err
		})
}

// The pinned SDK maps an unknown tool to a transport error. Keep write names
// out of the read catalog while returning a tool-level refusal for learned
// document-write names. See SDK server.go callTool and receiving middleware.
func refuseDocumentWrites(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); method == "tools/call" && ok && call.Params != nil && (call.Params.Name == "mutate" || call.Params.Name == "edit") {
			ae := &api.Error{Code: "forbidden", Message: "document mutation forbidden"}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: ae.Error()}}, StructuredContent: api.ErrorEnvelope{Version: api.Version, Error: ae}}, nil
		}
		return next(ctx, method, req)
	}
}

type mcpMutationArgs struct {
	api.Mutation
	IdempotencyKey string `json:"idempotency_key"`
}
type mcpEditArgs struct {
	store.EditRequest
	IdempotencyKey string `json:"idempotency_key"`
}

func addJocastaWriteTool[In any](srv *mcp.Server, name, description string, run func(context.Context, string, In) (any, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description},
		func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, any, error) {
			if req.Extra == nil || req.Extra.TokenInfo == nil {
				return nil, nil, errors.New("verified principal required")
			}
			p, ok := req.Extra.TokenInfo.Extra["principal"].(principal)
			if !ok || !p.canMutate() {
				return nil, nil, &api.Error{Code: "forbidden", Message: "document mutation forbidden"}
			}
			ctx, err := p.accessContext(ctx)
			if err != nil {
				return nil, nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := run(ctx, p.actor, args)
			return nil, out, err
		})
}
