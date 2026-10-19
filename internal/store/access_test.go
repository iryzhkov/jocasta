package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/jocasta/pkg/api"
)

func TestReadBoundsBeforeActivityHistoryAndMutations(t *testing.T) {
	s, e := Open(Config{Path: filepath.Join(t.TempDir(), "db"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, e := WithReadAccess(context.Background(), "issuer/sub", []string{"A"})
	if e != nil {
		t.Fatal(e)
	}
	create := func(path string) api.Document {
		t.Helper()
		content := "body"
		r, e := s.Mutate(context.Background(), "operator", path, api.Mutation{Operation: "create", Path: path, Content: &content})
		if e != nil {
			t.Fatal(e)
		}
		return r.Document
	}
	a := create("A/public")
	b := create("B/secret")
	if _, e = s.db.Exec("UPDATE activity SET last_active='original'"); e != nil {
		t.Fatal(e)
	}
	for _, ref := range []struct{ path, id string }{{"B/secret", ""}, {"", b.ID}} {
		if _, e = s.Get(ctx, ref.path, ref.id, 0); e == nil {
			t.Fatal("unauthorized get")
		}
	}
	var active string
	if e = s.db.QueryRow("SELECT last_active FROM activity WHERE id=?", b.ID).Scan(&active); e != nil || active != "original" {
		t.Fatalf("denied activity changed: %q %v", active, e)
	}
	if _, e = s.Get(ctx, "", a.ID, 0); e != nil {
		t.Fatal(e)
	}
	if e = s.db.QueryRow("SELECT last_active FROM activity WHERE id=?", a.ID).Scan(&active); e != nil || active == "original" {
		t.Fatal("authorized read failed to record activity", e)
	}
	var gen int64
	if e = s.db.QueryRow("SELECT generation FROM state").Scan(&gen); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Mutate(ctx, "operator", "A/public", api.Mutation{Operation: "create", Path: "A/public"}); e == nil {
		t.Fatal("read bounds bypassed by idempotency")
	}
	if e = s.Reindex(ctx); e == nil {
		t.Fatal("read-only reindex")
	}
	if e = s.Backup(ctx, filepath.Join(t.TempDir(), "backup")); e == nil {
		t.Fatal("read-only backup")
	}
	if _, e = s.ArchiveInactive(ctx, 1); e == nil {
		t.Fatal("read-only archive")
	}
	var after int64
	if e = s.db.QueryRow("SELECT generation FROM state").Scan(&after); e != nil || after != gen {
		t.Fatal("denied write changed generation")
	}
	history, e := s.Mutate(context.Background(), "operator", "move1", api.Mutation{Operation: "move", ID: b.ID, IfRevision: 1, Destination: "A/history"})
	if e != nil {
		t.Fatal(e)
	}
	page, e := s.History(ctx, "", history.Document.ID, 1, "")
	if e != nil || len(page.Documents) != 1 || page.Documents[0].Revision != 2 || page.ScannedCount != 0 || page.HasMore {
		t.Fatalf("history reveals hidden snapshot %+v %v", page, e)
	}
	if _, e = s.Get(ctx, "", b.ID, 1); e == nil {
		t.Fatal("historical denied get")
	}
}
func TestReadGrantsAndSearchDoNotObserveOtherProjects(t *testing.T) {
	for _, projects := range [][]string{nil, {}, {"*"}, {"A/B"}, {"_reserved"}, {".."}, {"e\u0301"}, {"A", string([]byte{255})}} {
		if _, e := WithReadAccess(context.Background(), "principal", projects); e == nil {
			t.Fatalf("accepted %q", projects)
		}
	}
	grants := []string{"A"}
	ctx, e := WithReadAccess(context.Background(), "issuer/sub", grants)
	if e != nil {
		t.Fatal(e)
	}
	grants[0] = "B"
	if !allowedPath(ctx, "A/doc") || allowedPath(ctx, "B/doc") {
		t.Fatal("mutable grant")
	}
	s, e := Open(Config{Path: filepath.Join(t.TempDir(), "db"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	create := func(path, body string) {
		t.Helper()
		if _, e := s.Mutate(context.Background(), "operator", path, api.Mutation{Operation: "create", Path: path, Content: &body}); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 3; i++ {
		create(fmt.Sprintf("A/doc%d", i), "needle visible")
	}
	filter := api.Filter{Query: "needle", Limit: 1}
	before, e := s.Page(ctx, filter, true)
	if e != nil {
		t.Fatal(e)
	}
	// More than the metadata candidate cap, with matching names and bodies,
	// must have no effect on visible counts, completeness, ranking or cursors.
	for i := 0; i < 505; i++ {
		create(fmt.Sprintf("B/needle%d", i), "needle denied")
	}
	after, e := s.Page(ctx, filter, true)
	if e != nil {
		t.Fatal(e)
	}
	bb, _ := json.Marshal(before)
	ba, _ := json.Marshal(after)
	if string(bb) != string(ba) {
		t.Fatalf("hidden corpus affected scoped search\nbefore %s\nafter %s", bb, ba)
	}
	if after.Ranking != "literal" || after.ScannedCount != 1 || !after.HasMore || after.CandidateLimit != 500 {
		t.Fatalf("%+v", after)
	}
	other, e := WithReadAccess(context.Background(), "different-subject", []string{"A"})
	if e != nil {
		t.Fatal(e)
	}
	bound := filter
	bound.Cursor = after.NextCursor
	if _, e = s.Page(other, bound, true); e == nil {
		t.Fatal("cursor accepted different principal")
	}
	// Even a cursor forged with an unscoped signature cannot change grants.
	raw, e := base64.RawURLEncoding.DecodeString(after.NextCursor)
	if e != nil {
		t.Fatal(e)
	}
	var forged cursor
	if e = json.Unmarshal(raw, &forged); e != nil {
		t.Fatal(e)
	}
	forged.Signature = Hash("forged")
	filter.Cursor = encode(forged)
	if _, e = s.Page(ctx, filter, true); e == nil {
		t.Fatal("cursor accepted foreign signature")
	}
	for _, query := range []string{"denied", "needle0", "B/", "needle"} {
		page, e := s.Page(ctx, api.Filter{Query: query, Limit: 200}, true)
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range page.Documents {
			if !strings.HasPrefix(d.Path, "A/") || strings.Contains(d.Content, "denied") {
				t.Fatalf("query leak %+v", page)
			}
		}
	}
}
