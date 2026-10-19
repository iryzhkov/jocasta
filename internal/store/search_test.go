package store

import (
	"context"
	"fmt"
	"github.com/iryzhkov/jocasta/pkg/api"
	"strings"
	"testing"
)

func TestCreatePreservesExistingProjection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	original := create(t, s, "project/first", "sharedtoken originaltoken")
	create(t, s, "project/second", "sharedtoken secondtoken")
	p, e := s.Page(ctx, api.Filter{Query: "sharedtoken"}, true)
	if e != nil || len(p.Documents) != 2 {
		t.Fatalf("create damaged projection: %+v %v", p, e)
	}
	p, e = s.Page(ctx, api.Filter{Query: "originaltoken"}, true)
	if e != nil || len(p.Documents) != 1 || p.Documents[0].ID != original.Document.ID || p.Documents[0].Revision != 1 {
		t.Fatalf("original projection changed: %+v %v", p, e)
	}
}
func TestUnicodeExcerpt(t *testing.T) {
	text := strings.Repeat("Ⱥ", 400) + "needle"
	if got := excerpt(text, "needle"); !strings.Contains(got, "needle") {
		t.Fatal(got)
	}
}
func TestIndexedSearchBeyondCandidateBudget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 520; i++ {
		create(t, s, fmt.Sprintf("corpus/%03d.md", i), "ordinary document")
	}
	// Choose the final random ID and modify it; reproduces a document beyond the
	// first 500 rows regardless of randomly generated IDs.
	var id string
	if e := s.db.QueryRow("SELECT id FROM documents ORDER BY id DESC LIMIT 1").Scan(&id); e != nil {
		t.Fatal(e)
	}
	body := "# Distinctive final document\nrarestphraseextra"
	if _, e := s.Mutate(ctx, "host", "unique", api.Mutation{Operation: "put", ID: id, IfRevision: 1, Content: &body}); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"Distinctive final document", "rarestphraseextra"} {
		p, e := s.Page(ctx, api.Filter{Query: q}, true)
		if e != nil || len(p.Documents) != 1 || p.Documents[0].ID != id {
			t.Fatalf("global indexed match %s: %+v %v", q, p, e)
		}
	}
	page, e := s.Page(ctx, api.Filter{Query: "ordinary", Limit: 17}, true)
	if e != nil {
		t.Fatal(e)
	}
	seen := map[string]bool{}
	for {
		for _, d := range page.Documents {
			if seen[d.ID] {
				t.Fatal("duplicate search result")
			}
			seen[d.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		page, e = s.Page(ctx, api.Filter{Query: "ordinary", Limit: 17, Cursor: page.NextCursor}, true)
		if e != nil {
			t.Fatal(e)
		}
	}
	if len(seen) != 519 {
		t.Fatalf("pagination returned %d", len(seen))
	}
}
