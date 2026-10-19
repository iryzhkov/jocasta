package store

import (
	"context"
	"database/sql"
	"github.com/iryzhkov/jocasta/pkg/api"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchemaOneMigrationBacksUpAndPreservesAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.db")
	s, e := Open(Config{Path: path, MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	content := "historicalneedle\r\nπ"
	m := api.Mutation{Operation: "create", Path: "demo/original.md", Content: &content}
	first, e := s.Mutate(ctx, "host", "create-key", m)
	if e != nil {
		t.Fatal(e)
	}
	moved, e := s.Mutate(ctx, "host", "move-key", api.Mutation{Operation: "move", ID: first.Document.ID, IfRevision: 1, Destination: "demo/current.md"})
	if e != nil {
		t.Fatal(e)
	}
	updated := "latestneedle"
	_, e = s.Mutate(ctx, "host", "update-key", api.Mutation{Operation: "put", ID: first.Document.ID, IfRevision: moved.Document.Revision, Content: &updated})
	if e != nil {
		t.Fatal(e)
	}
	// Recreate the exact v1 schema: mapping is the only schema2 addition.
	if _, e = s.db.Exec("DROP TABLE search_rows; PRAGMA user_version=1;"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(Config{Path: path, MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	backups, e := filepath.Glob(path + ".pre-v2.*.db")
	if e != nil || len(backups) != 1 {
		t.Fatalf("migration backup %v %v", backups, e)
	}
	backup, e := sql.Open("sqlite", backups[0])
	if e != nil {
		t.Fatal(e)
	}
	defer backup.Close()
	var version int
	if e = backup.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 1 {
		t.Fatalf("backup schema %d %v", version, e)
	}
	if e = s.db.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 2 {
		t.Fatalf("migrated schema %d %v", version, e)
	}
	got, e := s.Get(ctx, "demo/original.md", "", 0)
	if e != nil || !got.Alias || got.Document.ID != first.Document.ID || got.Document.Revision != 3 || got.Document.Content != updated {
		t.Fatalf("migration identity %+v %v", got, e)
	}
	old, e := s.Get(ctx, "", first.Document.ID, 1)
	if e != nil || old.Document.Content != content {
		t.Fatalf("historical bytes %+v %v", old, e)
	}
	receipt, e := s.Mutate(ctx, "host", "create-key", m)
	if e != nil || receipt.Document.Revision != 1 || receipt.Document.ID != first.Document.ID {
		t.Fatalf("receipt %+v %v", receipt, e)
	}
	for _, query := range []string{"latestneedle", "current.md", "original.md"} {
		p, e := s.Page(ctx, api.Filter{Query: query}, true)
		if e != nil || len(p.Documents) != 1 {
			t.Fatalf("migrated search %q %+v %v", query, p, e)
		}
	}
	p, e := s.Page(ctx, api.Filter{Query: "historicalneedle"}, true)
	if e != nil || len(p.Documents) != 0 {
		t.Fatalf("stale projection %+v %v", p, e)
	}
	if e = s.Reindex(ctx); e != nil {
		t.Fatal(e)
	}
	p, e = s.Page(ctx, api.Filter{Query: "latestneedle"}, true)
	if e != nil || len(p.Documents) != 1 {
		t.Fatalf("reindex mapping %+v %v", p, e)
	}
	var count int
	if e = s.db.QueryRow("SELECT count(*) FROM search_rows").Scan(&count); e != nil || count != 1 {
		t.Fatalf("mapping count %d %v", count, e)
	}
}
func TestNewerSchemaRefusalDoesNotChangeDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("PRAGMA user_version=99; CREATE TABLE future (value TEXT); INSERT INTO future VALUES('preserve');"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Open(Config{Path: path, MinFreeBytes: 1}); e == nil || !strings.Contains(e.Error(), "unsupported database schema") {
		t.Fatalf("newer schema accepted: %v", e)
	}
	after, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if string(before) != string(after) {
		t.Fatal("unsupported schema was mutated")
	}
}
