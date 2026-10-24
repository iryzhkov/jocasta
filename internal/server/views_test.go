package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iryzhkov/jocasta/internal/markdown"
	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
)

func TestMCPViewsAuthorizedBeforeLocatorAndPinnedExcerpts(t *testing.T) {
	f, o := newOperatorFixture(t)
	ctx := context.Background()
	create := func(path, body string) api.Document {
		t.Helper()
		r, e := f.store.Mutate(ctx, "local", path, api.Mutation{Operation: "create", Path: path, Content: &body})
		if e != nil {
			t.Fatal(e)
		}
		return r.Document
	}
	a := create("A/visible", "# Top\r\nπ needle\r\n## Child\nbody\n# Dup\nx\n# Dup\ny")
	b := create("B/secret", "# DENIED\n"+strings.Repeat("# x\n", markdown.MaxHeadings+1))
	hidden := create("A/oldalias", "# hidden\nneedle")
	_, e := f.store.Mutate(ctx, "local", "move-hidden", api.Mutation{Operation: "move", ID: hidden.ID, IfRevision: 1, Destination: "B/current"})
	if e != nil {
		t.Fatal(e)
	}
	history := create("B/old", "# DENIED history\nneedle")
	_, e = f.store.Mutate(ctx, "local", "move-visible", api.Mutation{Operation: "move", ID: history.ID, IfRevision: 1, Destination: "A/history"})
	if e != nil {
		t.Fatal(e)
	}
	clean := "# Safe\nneedle"
	_, e = f.store.Mutate(ctx, "local", "clean", api.Mutation{Operation: "put", ID: history.ID, IfRevision: 2, Content: &clean})
	if e != nil {
		t.Fatal(e)
	}
	bearer := &mcpBearerTransport{token: f.token(t, "A")}
	session := f.client(t, bearer)
	for _, args := range []map[string]any{
		{"path": "B/secret", "view": "outline"}, {"id": b.ID, "section": map[string]any{"heading": "DENIED"}},
		{"path": "A/oldalias", "view": "outline"}, {"id": hidden.ID, "revision": 1, "view": "outline"},
		{"path": "B/old", "section": map[string]any{"heading": "DENIED history"}},
		{"id": history.ID, "revision": 1, "view": "outline"},
	} {
		mcpError(t, session, "get", args, "not_found")
	}
	var outline outlineView
	if r := mcpCall(t, session, "get", map[string]any{"id": a.ID, "view": "outline", "levels": []int{2}}, &outline); r.IsError {
		t.Fatal(r)
	}
	if outline.ID != a.ID || outline.Revision != 1 || outline.ContentHash != a.ContentHash || len(outline.Headings) != 1 || outline.Headings[0].Text != "Child" || !outline.Complete {
		t.Fatal(outline)
	}
	var section sectionView
	mcpCall(t, session, "get", map[string]any{"id": a.ID, "section": map[string]any{"heading": "Top"}}, &section)
	if section.Content != "# Top\r\nπ needle\r\n## Child\nbody\n" || section.StartLine != 1 || section.SectionEndLine != 4 || !section.Complete {
		t.Fatal(section)
	}
	mcpError(t, session, "get", map[string]any{"id": a.ID, "section": map[string]any{"heading": "Dup"}}, "ambiguous_section")
	mcpCall(t, session, "get", map[string]any{"id": a.ID, "section": map[string]any{"heading": "Dup", "heading_line": 5}}, &section)
	if section.Content != "# Dup\nx\n" {
		t.Fatal(section)
	}
	mcpError(t, session, "get", map[string]any{"id": a.ID, "section": map[string]any{"heading": "Dup", "heading_line": 6}}, "section_not_found")
	for _, args := range []map[string]any{
		{"id": a.ID, "view": "outline", "lines": "1:2"}, {"id": a.ID, "view": "outline", "section": map[string]any{"heading": "Top"}},
		{"id": a.ID, "view": "outline", "levels": []int{0, 7}}, {"id": a.ID, "levels": []int{1}},
	} {
		mcpError(t, session, "get", args, "invalid")
	}
	var enriched enrichedPage
	if r := mcpCall(t, session, "search", map[string]any{"q": "needle", "include_excerpts": true, "excerpt_bytes": 16}, &enriched); r.IsError {
		t.Fatal(r)
	}
	if len(enriched.Documents) != 2 || len(enriched.Excerpts) != 2 {
		t.Fatal(enriched)
	}
	raw, _ := json.Marshal(enriched)
	if strings.Contains(string(raw), "DENIED") || strings.Contains(string(raw), "B/") {
		t.Fatal("ACL excerpt leak", string(raw))
	}
	for _, ex := range enriched.Excerpts {
		r, e := f.store.Get(ctx, "", ex.ID, ex.Revision)
		if e != nil {
			t.Fatal(e)
		}
		if r.FullDocumentHash != ex.ContentHash || r.Document.Content[ex.StartByte:ex.EndByte] != ex.Content || len(ex.Content) > 16 || ex.Heading == nil {
			t.Fatal(ex)
		}
	}
	// A later write cannot change the excerpt's pinned revision bytes.
	changed := "# Changed\nother"
	_, e = f.store.Mutate(ctx, "local", "later", api.Mutation{Operation: "put", ID: a.ID, IfRevision: 1, Content: &changed})
	if e != nil {
		t.Fatal(e)
	}
	mcpCall(t, session, "get", map[string]any{"id": a.ID, "revision": 1, "section": map[string]any{"heading": "Top"}}, &section)
	if section.Revision != 1 || !strings.Contains(section.Content, "needle") {
		t.Fatal(section)
	}
	mcpError(t, session, "get", map[string]any{"id": a.ID, "section": map[string]any{"heading": "Top", "heading_line": 1}}, "section_not_found")
	// Operator can read the historical denied project. Limited history remains filtered.
	bearer.set(signed(t, o.key, operatorClaims(f, o, false)))
	mcpCall(t, session, "get", map[string]any{"id": history.ID, "revision": 1, "section": map[string]any{"heading": "DENIED history"}}, &section)
	if !strings.Contains(section.Content, "DENIED history") {
		t.Fatal(section)
	}
}
func TestViewBudgetsAndExactReferenceReceipts(t *testing.T) {
	f := newMCPFixture(t)
	ctx, _ := store.WithReadAccess(context.Background(), "limited", []string{"A"})
	create := func(path, body string) api.Document {
		t.Helper()
		r, e := f.store.Mutate(context.Background(), "local", path, api.Mutation{Operation: "create", Path: path, Content: &body})
		if e != nil {
			t.Fatal(e)
		}
		return r.Document
	}
	body := "# Huge\r\n" + strings.Repeat("π\r\n\u0001", 20000)
	d := create("A/huge", body)
	result, e := f.server.getView(ctx, mcpGetArgs{ID: d.ID, Section: &markdown.Selector{Heading: "Huge"}})
	if e != nil {
		t.Fatal(e)
	}
	v := result.(sectionView)
	if !v.Truncated || v.Complete || v.NextLines == "" || jsonSize(v) > viewBudget || !utf8.ValidString(v.Content) || body[v.StartByte:v.EndByte] != v.Content || v.ContentHash != store.Hash(body) {
		t.Fatal("dishonest section receipt", v)
	}
	many := strings.Repeat("# heading\n", 250)
	d = create("A/many", many)
	result, e = f.server.getView(ctx, mcpGetArgs{ID: d.ID, View: "outline"})
	if e != nil {
		t.Fatal(e)
	}
	ov := result.(outlineView)
	if ov.ReturnedHeadings != 200 || ov.TotalHeadings != 250 || ov.Complete || !ov.Truncated || ov.NextLines != "201:250" || jsonSize(ov) > viewBudget {
		t.Fatal(ov)
	}
	enormous := "# " + strings.Repeat("x", viewBudget) + "\nbody"
	d = create("A/label", enormous)
	result, e = f.server.getView(ctx, mcpGetArgs{ID: d.ID, View: "outline"})
	if e != nil {
		t.Fatal(e)
	}
	ov = result.(outlineView)
	if len(ov.Headings) != 0 || !ov.Truncated || ov.NextLines == "" {
		t.Fatal(ov)
	}
	// Literal excerpt positions retain byte coordinates for non-ASCII text.
	d = create("A/π-name", "# Heading\nπ needle\r\nend")
	n := 20
	result, e = f.server.searchView(ctx, mcpPageArgs{Query: "needle", IncludeExcerpts: true, ExcerptBytes: 9, Limit: &n})
	if e != nil {
		t.Fatal(e)
	}
	page := result.(enrichedPage)
	if len(page.Excerpts) != 1 {
		t.Fatal(page)
	}
	ex := page.Excerpts[0]
	if ex.StartLine != 2 || ex.EndLine != 2 || ex.Content != "π needle" || ex.Revision != 1 || ex.ContentHash != d.ContentHash {
		t.Fatal(ex)
	}
	// Metadata-only exact names say no literal body match.
	result, e = f.server.searchView(ctx, mcpPageArgs{Query: "π-name", IncludeExcerpts: true})
	if e != nil {
		t.Fatal(e)
	}
	ex = result.(enrichedPage).Excerpts[0]
	if ex.Content != "" || ex.Reason != "no_exact_literal_match" {
		t.Fatal(ex)
	}
	for _, a := range []mcpPageArgs{{Query: "needle", IncludeExcerpts: true, Limit: intPtr(21)}, {Query: "needle", IncludeExcerpts: true, ExcerptBytes: 2049}, {Query: "needle", ExcerptBytes: 10}} {
		if _, e = f.server.searchView(ctx, a); e == nil {
			t.Fatal(fmt.Sprint(a))
		}
	}
}
func intPtr(n int) *int { return &n }
