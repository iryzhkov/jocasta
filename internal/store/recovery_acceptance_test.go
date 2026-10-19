package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iryzhkov/jocasta/pkg/api"
)

// Exercise a real SQLite backup as an independent recovery boundary, then discard
// the search projection to ensure recovery rebuilds it from authoritative data.
func TestRecoveryAcceptanceIdentityAliasesHistoryReceipts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	original := "# Recovery plan\r\n雪 without final newline"
	createRequest := api.Mutation{Operation: "create", Path: "project/old.md", Content: ptr(original), Title: ptr("Recovery plan"), Kind: ptr("plan")}
	first, e := s.Mutate(ctx, "host-a", "create-recovery", createRequest)
	if e != nil {
		t.Fatal(e)
	}
	id := first.Document.ID
	mutate := func(key string, m api.Mutation) api.Receipt {
		t.Helper()
		r, e := s.Mutate(ctx, "host-a", key, m)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	moveRequest := api.Mutation{Operation: "move", ID: id, IfRevision: 1, Destination: "project/new.md"}
	moved := mutate("move-recovery", moveRequest)
	mutate("update-recovery", api.Mutation{Operation: "put", ID: id, IfRevision: 2, Content: ptr("updated\n")})
	mutate("archive-recovery", api.Mutation{Operation: "archive", ID: id, IfRevision: 3})
	mutate("restore-recovery", api.Mutation{Operation: "restore", ID: id, IfRevision: 4, SourceRevision: 1})
	backup := filepath.Join(t.TempDir(), "recovered.sqlite")
	if e = s.Backup(ctx, backup); e != nil {
		t.Fatal(e)
	}
	if e = s.Backup(ctx, backup); e == nil {
		t.Fatal("existing backup overwritten")
	}
	info, e := os.Stat(backup)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %v %v", info, e)
	}
	recovered, e := Open(Config{Path: backup, MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close()
	// This deliberate projection damage is confined to the restored test copy.
	if _, e = recovered.db.Exec("DELETE FROM search"); e != nil {
		t.Fatal(e)
	}
	if e = recovered.Reindex(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := recovered.Get(ctx, "project/old.md", "", 0)
	if e != nil {
		t.Fatal(e)
	}
	if !got.Alias || got.Document.ID != id || got.Document.Path != "project/new.md" || got.Document.Revision != 5 || !got.Document.Archived || got.Document.Content != original {
		t.Fatalf("identity/lifecycle lost: %+v", got)
	}
	for rev := int64(1); rev <= 5; rev++ {
		a, e := s.Get(ctx, "", id, rev)
		if e != nil {
			t.Fatal(e)
		}
		b, e := recovered.Get(ctx, "", id, rev)
		if e != nil {
			t.Fatal(e)
		}
		if a.Document != b.Document {
			t.Fatalf("revision %d changed across backup", rev)
		}
	}
	replay, e := recovered.Mutate(ctx, "host-a", "move-recovery", moveRequest)
	if e != nil || replay.Document != moved.Document {
		t.Fatalf("lost original receipt: %+v %v", replay, e)
	}
	_, e = recovered.Mutate(ctx, "host-a", "move-recovery", api.Mutation{Operation: "archive", ID: id, IfRevision: 5})
	if code(e) != "idempotency_conflict" {
		t.Fatalf("receipt payload guard lost: %v", e)
	}
	_, e = recovered.Mutate(ctx, "host-b", "collision", createRequest)
	if code(e) != "conflict" {
		t.Fatalf("alias reservation lost: %v", e)
	}
	page, e := recovered.Page(ctx, api.Filter{Query: "Recovery plan", Limit: 20}, true)
	if e != nil || len(page.Documents) != 1 || page.Documents[0].ID != id || !page.Documents[0].Archived {
		t.Fatalf("rebuilt search: %+v %v", page, e)
	}
	history, e := recovered.History(ctx, "", id, 2, "")
	if e != nil || len(history.Documents) != 2 || history.NextCursor == "" {
		t.Fatalf("history first page: %+v %v", history, e)
	}
	after, e := recovered.History(ctx, "", id, 2, history.NextCursor)
	if e != nil || len(after.Documents) != 2 || after.Documents[0].Revision != 3 {
		t.Fatalf("history continuation: %+v %v", after, e)
	}
}
func TestRecoveryAcceptanceExpandedJSONBudgetRollback(t *testing.T) {
	s := testStore(t)
	var size int64
	for _, path := range []string{s.cfg.Path, s.cfg.Path + "-wal", s.cfg.Path + "-shm"} {
		if st, e := os.Stat(path); e == nil {
			size += st.Size()
		}
	}
	// Control characters are valid document UTF-8 but expand sixfold in stored JSON.
	s.cfg.MaxStorageBytes = size + (128 << 10)
	_, e := s.Mutate(context.Background(), "host", "expanded-budget", api.Mutation{Operation: "create", Path: "project/expanded", Content: ptr(strings.Repeat("\x01", 4096))})
	if code(e) != "storage_full" {
		t.Fatalf("JSON storage expansion not budgeted: %v", e)
	}
	for _, table := range []string{"documents", "revisions", "paths", "receipts", "search"} {
		var n int
		if e = s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil || n != 0 {
			t.Fatalf("%s partial state count=%d err=%v", table, n, e)
		}
	}
}
func TestRecoveryAcceptanceHistoryAllocationBound(t *testing.T) {
	if testing.Short() {
		t.Skip("large revision history resource regression")
	}
	s := testStore(t)
	ctx := context.Background()
	content := strings.Repeat("x", 1<<20)
	first := create(t, s, "project/history", content)
	for n := int64(1); n < 16; n++ {
		next := content + fmt.Sprint(n)
		if _, e := s.Mutate(ctx, "host", fmt.Sprint(n), api.Mutation{Operation: "put", ID: first.Document.ID, IfRevision: n, Content: &next}); e != nil {
			t.Fatal(e)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	history, e := s.History(ctx, "", first.Document.ID, 200, "")
	runtime.ReadMemStats(&after)
	if e != nil || len(history.Documents) != 16 {
		t.Fatalf("history %+v %v", history, e)
	}
	for _, d := range history.Documents {
		if d.Content != "" {
			t.Fatal("history exposed full content")
		}
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 20<<20 {
		t.Fatalf("metadata history allocated %d bytes; full snapshots must stay out of Go", allocated)
	}
}
func TestRecoveryAcceptanceNewlineRangeAllocationBound(t *testing.T) {
	content := strings.Repeat("\n", 16<<20)
	r := api.Receipt{Document: api.Document{Content: content, Revision: 9}, TotalLines: len(content), FullDocumentHash: Hash(content)}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, e := Range(r, "16777216:16777220")
	runtime.ReadMemStats(&after)
	if e != nil || got.Document.Content != "\n" || got.ReturnedLines != "16777216:16777216" || got.FullDocumentHash != r.FullDocumentHash || !got.Partial {
		t.Fatalf("range boundary failed: %v", e)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("range allocated %d bytes for one-line excerpt", allocated)
	}
}
