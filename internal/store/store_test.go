package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/iryzhkov/jocasta/pkg/api"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(Config{Path: filepath.Join(t.TempDir(), "db.sqlite"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func ptr(s string) *string { return &s }
func create(t *testing.T, s *Store, path, body string) api.Receipt {
	t.Helper()
	r, e := s.Mutate(context.Background(), "host", path, api.Mutation{Operation: "create", Path: path, Content: ptr(body)})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func code(e error) string {
	var a *api.Error
	if errors.As(e, &a) {
		return a.Code
	}
	return ""
}
func TestConcurrencyRetryLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	source := "# Heading\r\nπ \r\n"
	original := create(t, s, "demo/plans/one.md", source)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := s.Mutate(ctx, "host", fmt.Sprint(i), api.Mutation{Operation: "put", ID: original.Document.ID, IfRevision: 1, Content: ptr(fmt.Sprint(i))})
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if code(e) == "conflict" {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	r, e := s.Mutate(ctx, "host", "demo/plans/one.md", api.Mutation{Operation: "create", Path: "demo/plans/one.md", Content: ptr(source)})
	if e != nil || r.Document.Revision != 1 {
		t.Fatalf("lost response replay: %+v %v", r, e)
	}
	_, e = s.Mutate(ctx, "host", "demo/plans/one.md", api.Mutation{Operation: "create", Path: "other/path", Content: ptr("x")})
	if code(e) != "idempotency_conflict" {
		t.Fatal(e)
	}
	moved, e := s.Mutate(ctx, "host", "move", api.Mutation{Operation: "move", ID: r.Document.ID, IfRevision: 2, Destination: "demo/moved.md"})
	if e != nil {
		t.Fatal(e)
	}
	alias, e := s.Get(ctx, "demo/plans/one.md", "", 0)
	if e != nil || !alias.Alias || alias.Document.Path != "demo/moved.md" {
		t.Fatalf("%+v %v", alias, e)
	}
	archived, e := s.Mutate(ctx, "host", "archive", api.Mutation{Operation: "archive", ID: r.Document.ID, IfRevision: moved.Document.Revision})
	if e != nil {
		t.Fatal(e)
	}
	restored, e := s.Mutate(ctx, "host", "restore", api.Mutation{Operation: "restore", ID: r.Document.ID, IfRevision: archived.Document.Revision, SourceRevision: 1})
	if e != nil {
		t.Fatal(e)
	}
	// A write answers with metadata alone, so the restored bytes are read back.
	current, e := s.Get(ctx, "", original.Document.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	if !restored.Document.Archived || restored.Document.Path != "demo/moved.md" || current.Document.Content != source {
		t.Fatalf("%+v %+v", restored, current)
	}
	before := restored.Document.Revision
	noop, e := s.Mutate(ctx, "host", "noop", api.Mutation{Operation: "put", ID: r.Document.ID, IfRevision: before, Content: ptr(current.Document.Content)})
	if e != nil || noop.Document.Revision != before {
		t.Fatalf("no-op: %+v %v", noop, e)
	}
}
func TestBoundsRangesBytesAndSearch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i, body := range []string{"", "a", "a\n", "a\r\nβ\r\n", "  π\t", strings.Repeat("x", 1<<20)} {
		r := create(t, s, fmt.Sprintf("bytes/%d", i), body)
		got, e := s.Get(ctx, "", r.Document.ID, 0)
		if e != nil || got.Document.Content != body {
			t.Fatalf("bytes %d %v", i, e)
		}
	}
	r := create(t, s, "plans/Jocasta.md", "# Jocasta\nAgent workshop\nhttps://x.test/a_b?q=x\n")
	for _, q := range []string{"Jocsta", "Agent workshop", "plans/Jocasta", "https://x.test/a_b?q=x"} {
		mode := "default"
		if strings.HasPrefix(q, "https") {
			mode = "literal"
		}
		p, e := s.Page(ctx, api.Filter{Query: q, Mode: mode}, true)
		if e != nil || len(p.Documents) != 1 {
			t.Fatalf("search %s: %+v %v", q, p, e)
		}
	}
	full, e := s.Get(ctx, "", r.Document.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	ranged, e := Range(full, "2:900")
	if e != nil || ranged.Document.Content != "Agent workshop\nhttps://x.test/a_b?q=x\n" || ranged.ReturnedLines != "2:3" || ranged.FullDocumentHash != r.Document.ContentHash {
		t.Fatalf("%+v %v", ranged, e)
	}
	for _, value := range []string{"0:1", "3:2", "4:5"} {
		if _, e = Range(full, value); e == nil {
			t.Fatal(value)
		}
	}
	for _, content := range []string{"a\x00b", "\xff", strings.Repeat("z", 16<<20+1)} {
		_, e = s.Mutate(ctx, "host", Hash(content), api.Mutation{Operation: "create", Path: "bad/content", Content: ptr(content)})
		if code(e) != "invalid_input" {
			t.Fatalf("invalid content %v", e)
		}
	}
	for _, path := range []string{"../evil", "project/../evil", "/project/doc", "project//doc", "project\\doc", "_reserved/doc"} {
		if _, e = NormalizePath(path); e == nil {
			t.Fatal(path)
		}
	}
	p, e := s.Page(ctx, api.Filter{Limit: 1}, false)
	if e != nil || p.NextCursor == "" {
		t.Fatalf("%+v %v", p, e)
	}
	create(t, s, "other/write", "x")
	_, e = s.Page(ctx, api.Filter{Limit: 1, Cursor: p.NextCursor}, false)
	if code(e) != "cursor_expired" {
		t.Fatal(e)
	}
}
func TestBackupReindexAndInactivity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	source := "# Retention\nsecretbody"
	r := create(t, s, "demo/retention.md", source)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, e := s.db.Exec("UPDATE activity SET last_active=?", old); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get(ctx, "", r.Document.ID, 1); e != nil {
		t.Fatal(e)
	}
	if n, e := s.ArchiveInactive(ctx, 30); e != nil || n != 0 {
		t.Fatalf("read reset %d %v", n, e)
	}
	s.db.Exec("UPDATE activity SET last_active=?", old)
	if n, e := s.ArchiveInactive(ctx, 30); e != nil || n != 1 {
		t.Fatalf("archive %d %v", n, e)
	}
	p, e := s.Page(ctx, api.Filter{Query: "retention"}, true)
	if e != nil || len(p.Documents) != 1 || !p.Documents[0].Archived {
		t.Fatalf("archived name %+v %v", p, e)
	}
	p, e = s.Page(ctx, api.Filter{Query: "secretbody"}, true)
	if e != nil || len(p.Documents) != 0 {
		t.Fatalf("archived content %+v %v", p, e)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if e = s.Backup(ctx, backup); e != nil {
		t.Fatal(e)
	}
	recovered, e := Open(Config{Path: backup, MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close()
	if e = recovered.Reindex(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := recovered.Get(ctx, "demo/retention.md", "", 1)
	if e != nil || got.Document.Content != source {
		t.Fatalf("%+v %v", got, e)
	}
	replay, e := recovered.Mutate(ctx, "host", "demo/retention.md", api.Mutation{Operation: "create", Path: "demo/retention.md", Content: ptr(source)})
	if e != nil || replay.Document.Revision != 1 {
		t.Fatalf("%+v %v", replay, e)
	}
}
func TestStorageFullRollsBack(t *testing.T) {
	s := testStore(t)
	s.cfg.MaxStorageBytes = 1
	_, e := s.Mutate(context.Background(), "host", "full", api.Mutation{Operation: "create", Path: "demo/full", Content: ptr("x")})
	if code(e) != "storage_full" {
		t.Fatal(e)
	}
	var count int
	s.db.QueryRow("SELECT count(*) FROM revisions").Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
}
