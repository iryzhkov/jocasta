package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/iryzhkov/jocasta/internal/markdown"
	"github.com/iryzhkov/jocasta/pkg/api"
)

func errorCode(e error) string {
	var a *api.Error
	if errors.As(e, &a) {
		return a.Code
	}
	return fmt.Sprint(e)
}
func TestOperatorEditTransactionReplayAndIsolation(t *testing.T) {
	s, e := Open(Config{Path: filepath.Join(t.TempDir(), "db"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, _ := WithOperatorAccess(context.Background(), "human", true)
	body := "# First\r\nπ old\r\n## Child\r\nuntouched\r\n# Last\nold"
	created, e := s.Mutate(ctx, "human", "create", api.Mutation{Operation: "create", Path: "future/exact", Content: &body})
	if e != nil {
		t.Fatal(e)
	}
	edit := EditRequest{ID: created.Document.ID, IfRevision: 1, Old: "old", New: "new", Section: &markdown.Selector{Heading: "First", HeadingLine: 1}, Message: "exact"}
	first, e := s.Edit(ctx, "human", "edit", edit)
	if e != nil {
		t.Fatal(e)
	}
	if first.Document.Revision != 2 || first.Document.Actor != "human" || first.Document.Content != "" {
		t.Fatal(first)
	}
	got, e := s.Get(ctx, "", created.Document.ID, 0)
	if e != nil || got.Document.Content != strings.Replace(body, "old", "new", 1) {
		t.Fatalf("%q %v", got.Document.Content, e)
	}
	later := "later"
	_, e = s.Mutate(ctx, "human", "later", api.Mutation{Operation: "put", ID: created.Document.ID, IfRevision: 2, Content: &later})
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.Edit(ctx, "human", "edit", edit)
	if e != nil || !reflect.DeepEqual(replay, first) {
		t.Fatal("replay before current state", replay, e)
	}
	changed := edit
	changed.New = "different"
	if _, e = s.Edit(ctx, "human", "edit", changed); errorCode(e) != "idempotency_conflict" {
		t.Fatal(e)
	}
	// Receipt lookup is actor-bound, and a caller cannot override its actor.
	if _, e = s.Edit(ctx, "other", "edit", edit); errorCode(e) != "forbidden" {
		t.Fatal(e)
	}
	other, _ := WithOperatorAccess(context.Background(), "other", true)
	if _, e = s.Edit(other, "other", "edit", edit); errorCode(e) != "conflict" {
		t.Fatal("cross actor replay", e)
	}
	limited, _ := WithReadAccess(context.Background(), "human", []string{"future"})
	readonly, _ := WithOperatorAccess(context.Background(), "human", false)
	for _, c := range []context.Context{limited, readonly} {
		if _, e = s.Edit(c, "human", "edit", edit); errorCode(e) != "forbidden" {
			t.Fatal("receipt bypass", e)
		}
		if _, e = s.Mutate(c, "human", "create", api.Mutation{}); errorCode(e) != "forbidden" {
			t.Fatal(e)
		}
	}
	if e = s.Reindex(ctx); errorCode(e) != "forbidden" {
		t.Fatal("maintenance broadened", e)
	}
	if e = s.Backup(ctx, filepath.Join(t.TempDir(), "backup")); errorCode(e) != "forbidden" {
		t.Fatal(e)
	}
	if _, e = s.ArchiveInactive(ctx, 30); errorCode(e) != "forbidden" {
		t.Fatal(e)
	}
	// Legacy request digests remain the exact JSON hash.
	raw, _ := json.Marshal(api.Mutation{Operation: "create", Path: "future/exact", Content: &body})
	var digest string
	if e = s.db.QueryRow("SELECT digest FROM receipts WHERE actor='human' AND key='create'").Scan(&digest); e != nil || digest != Hash(string(raw)) {
		t.Fatal("legacy digest changed", e)
	}
	var revisions, receipts int
	s.db.QueryRow("SELECT count(*) FROM revisions").Scan(&revisions)
	s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts)
	if revisions != 3 || receipts != 3 {
		t.Fatal("refusal/replay created rows", revisions, receipts)
	}
}
func TestEditUniqueMatchesCASAndCancellation(t *testing.T) {
	s, e := Open(Config{Path: filepath.Join(t.TempDir(), "db"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, _ := WithOperatorAccess(context.Background(), "human", true)
	body := "# Dup\naaa\n# Dup\nold\n"
	r, e := s.Mutate(ctx, "human", "c", api.Mutation{Operation: "create", Path: "A/doc", Content: &body})
	if e != nil {
		t.Fatal(e)
	}
	base := EditRequest{ID: r.Document.ID, IfRevision: 1, Old: "aa", New: "x"}
	for i, edit := range []EditRequest{base, {ID: r.Document.ID, IfRevision: 1, Old: "missing", New: "x"}, {ID: r.Document.ID, IfRevision: 1, Old: "old", New: "x", Section: &markdown.Selector{Heading: "Dup"}}, {ID: r.Document.ID, IfRevision: 1, Old: "", New: "x"}} {
		if _, e = s.Edit(ctx, "human", fmt.Sprint(i), edit); e == nil {
			t.Fatal("ambiguous/missing accepted", edit)
		}
	}
	base.Old = "old"
	base.New = ""
	var wg sync.WaitGroup
	codes := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Edit(ctx, "human", fmt.Sprint("race", i), base)
			codes <- errorCode(err)
		}(i)
	}
	wg.Wait()
	close(codes)
	ok, conflicts := 0, 0
	for c := range codes {
		if c == "<nil>" {
			ok++
		}
		if c == "conflict" {
			conflicts++
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatal(ok, conflicts)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.Edit(canceled, "human", "cancel", base); e == nil {
		t.Fatal("cancelled edit committed")
	}
	got, e := s.Get(ctx, "", r.Document.ID, 0)
	if e != nil || got.Document.Content != "# Dup\naaa\n# Dup\n\n" || got.Document.Revision != 2 {
		t.Fatal(got, e)
	}
}
func TestOperatorSearchAndCursorPermissions(t *testing.T) {
	s, e := Open(Config{Path: filepath.Join(t.TempDir(), "db"), MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, _ := WithOperatorAccess(context.Background(), "human", true)
	for _, path := range []string{"A/one", "B/two", "future/three"} {
		body := "exactneedle"
		if _, e = s.Mutate(ctx, "human", path, api.Mutation{Operation: "create", Path: path, Content: &body}); e != nil {
			t.Fatal(e)
		}
	}
	plain, e := s.Page(context.Background(), api.Filter{Query: "exactneedle"}, true)
	if e != nil {
		t.Fatal(e)
	}
	wide, e := s.Page(ctx, api.Filter{Query: "exactneedle"}, true)
	if e != nil || wide.Ranking != plain.Ranking || !reflect.DeepEqual(wide.Documents, plain.Documents) {
		t.Fatal("operator lost full search", wide, e)
	}
	for _, search := range []bool{false, true} {
		f := api.Filter{Limit: 1}
		if search {
			f.Query = "exactneedle"
		}
		p, e := s.Page(ctx, f, search)
		if e != nil || !p.HasMore {
			t.Fatal(p, e)
		}
		f.Cursor = p.NextCursor
		renewed, _ := WithOperatorAccess(context.Background(), "human", true)
		if _, e = s.Page(renewed, f, search); e != nil {
			t.Fatal("same grants renewal invalidated", e)
		}
		limited, _ := WithReadAccess(context.Background(), "human", []string{"A", "B", "future"})
		narrow, _ := WithReadAccess(context.Background(), "human", []string{"A"})
		ro, _ := WithOperatorAccess(context.Background(), "human", false)
		other, _ := WithOperatorAccess(context.Background(), "other", true)
		for _, c := range []context.Context{context.Background(), limited, narrow, ro, other} {
			if _, e = s.Page(c, f, search); e == nil {
				t.Fatal("cross permission cursor")
			}
		}
	}
}
