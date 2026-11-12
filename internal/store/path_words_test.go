package store

import (
	"context"
	"testing"

	"github.com/iryzhkov/jocasta/pkg/api"
)

func TestSearchPathWords(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(map[bool]string{false: "indexed", true: "without_index"}[degraded], func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			want := create(t, s, "tooling/guides/web-mcp-authentik-authoring-2026-10-19.md", "# Integration recipe\n")
			create(t, s, "tooling/guides/webmcpart.md", "# Unrelated\n")
			other := create(t, s, "tooling/word_one.two-three/four.md", "Unrelated body")
			if degraded {
				if _, err := s.db.Exec("DROP TABLE search"); err != nil {
					t.Fatal(err)
				}
			}
			for query, id := range map[string]string{
				"web mcp":                 want.Document.ID,
				"word one two three four": other.Document.ID,
			} {
				page, err := s.Page(ctx, api.Filter{Query: query}, true)
				if err != nil || len(page.Documents) != 1 || page.Documents[0].ID != id {
					t.Fatalf("query %q: %+v, %v", query, page, err)
				}
			}
			limited, err := WithReadAccess(ctx, "reader", []string{"tooling"})
			if err != nil {
				t.Fatal(err)
			}
			page, err := s.Page(limited, api.Filter{Query: "web mcp"}, true)
			if err != nil || len(page.Documents) != 1 || page.Documents[0].ID != want.Document.ID {
				t.Fatalf("limited path words: %+v, %v", page, err)
			}
			if _, err := s.Mutate(ctx, "host", "archive-path", api.Mutation{Operation: "archive", ID: want.Document.ID, IfRevision: 1}); err != nil {
				t.Fatal(err)
			}
			page, err = s.Page(ctx, api.Filter{Query: "web mcp"}, true)
			if err != nil || len(page.Documents) != 1 || page.Documents[0].ID != want.Document.ID {
				t.Fatalf("archived path words: %+v, %v", page, err)
			}
		})
	}
}
