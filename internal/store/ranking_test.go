package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iryzhkov/jocasta/pkg/api"
)

// corpus builds the shape the audit found: one document that is the framework,
// and three that merely cite it by name and path. The framework's path is
// hyphenated and its derived title is not the query verbatim, which is what
// dropped every candidate into the base implementation's last tier.
func corpus(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	create(t, s, "shared/references/agent-experience-framework.md",
		"# Agent-Experience Framework\n\nPrinciples for the tools that agents use.\n")
	create(t, s, "shared/notes/tooling-review.md",
		"# Tooling review\n\nMeasured against the agent experience framework in\nshared/references/agent-experience-framework.md and its principles.\n")
	create(t, s, "shared/notes/audit-two.md",
		"# Audit two\n\nThe agent experience framework says a refusal names the fix.\nSee shared/references/agent-experience-framework.md for the principles.\n")
	create(t, s, "shared/notes/campaign.md",
		"# Campaign\n\nStage four cites the agent experience framework twice:\nagent experience framework, agent experience framework.\n")
	return s
}

// A document whose own title and path match the query ranks first, ahead of the
// documents that only cite it. The base implementation put every candidate of a
// multi-word query into one tier and tie-broke on the random hex ID, so it
// returned an arbitrary order; repeating the corpus makes an accidental pass
// vanishingly unlikely rather than one chance in four.
func TestSearchRanksTheNamedDocumentFirst(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 8; attempt++ {
		s := corpus(t)
		p, e := s.Page(ctx, api.Filter{Query: "agent experience framework", Limit: 20}, true)
		if e != nil {
			t.Fatal(e)
		}
		if len(p.Documents) < 2 {
			t.Fatalf("attempt %d matched %d documents", attempt, len(p.Documents))
		}
		order := make([]string, 0, len(p.Documents))
		for _, d := range p.Documents {
			order = append(order, d.Path)
		}
		if order[0] != "shared/references/agent-experience-framework.md" {
			t.Fatalf("attempt %d ranked the framework document %d of %d: %v", attempt, 1+indexOf(order, "shared/references/agent-experience-framework.md"), len(order), order)
		}
		cited := indexOf(order, "shared/notes/tooling-review.md")
		if cited == 0 {
			t.Fatalf("attempt %d ranked a citing document first: %v", attempt, order)
		}
	}
}
func indexOf(all []string, want string) int {
	for i, v := range all {
		if v == want {
			return i
		}
	}
	return -1
}

// bm25 scores are relative to the corpus, so the order between two documents is
// not promised to survive a corpus change. What a rebuild of the same corpus
// must not change is which document answers the query, including after the
// update churn that leaves deleted rows behind in the projection.
func TestRankingSurvivesAReindexOfTheSameCorpus(t *testing.T) {
	ctx := context.Background()
	s := corpus(t)
	for revision := int64(1); revision <= 4; revision++ {
		body := strings.Repeat("churn ", int(revision)) + "agent experience framework mentioned again\n"
		p, e := s.Page(ctx, api.Filter{Query: "campaign", Limit: 5}, true)
		if e != nil || len(p.Documents) == 0 {
			t.Fatalf("%+v %v", p, e)
		}
		if _, e = s.Mutate(ctx, "host", "churn"+string(rune('a'+revision)), api.Mutation{Operation: "put", ID: p.Documents[0].ID, IfRevision: revision, Content: &body}); e != nil {
			t.Fatal(e)
		}
	}
	before, e := s.Page(ctx, api.Filter{Query: "agent experience framework", Limit: 20}, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Reindex(ctx); e != nil {
		t.Fatal(e)
	}
	after, e := s.Page(ctx, api.Filter{Query: "agent experience framework", Limit: 20}, true)
	if e != nil {
		t.Fatal(e)
	}
	if len(before.Documents) != len(after.Documents) || len(after.Documents) == 0 {
		t.Fatalf("rebuild changed the result set: %d then %d", len(before.Documents), len(after.Documents))
	}
	if after.Documents[0].Path != "shared/references/agent-experience-framework.md" {
		t.Fatalf("rebuild changed which document answers the query: %s", after.Documents[0].Path)
	}
	for i := range before.Documents {
		if before.Documents[i].ID != after.Documents[i].ID {
			t.Logf("note: a rebuild reordered position %d; only the first result is promised", i+1)
			break
		}
	}
}

// Titles are derived on write and indexed, so a search hit can carry the title
// a reader would recognise instead of the empty string an unset title leaves.
func TestSearchHitCarriesItsDerivedTitle(t *testing.T) {
	s := corpus(t)
	p, e := s.Page(context.Background(), api.Filter{Query: "agent experience framework", Limit: 20}, true)
	if e != nil || len(p.Documents) == 0 {
		t.Fatalf("%+v %v", p, e)
	}
	for _, d := range p.Documents {
		if d.Title == "" {
			t.Fatalf("search hit %s has no title", d.Path)
		}
	}
	if p.Documents[0].Title != "Agent-Experience Framework" {
		t.Fatalf("derived title lost: %q", p.Documents[0].Title)
	}
}

// When the index cannot answer, search says so rather than returning an order
// nobody can explain. Asserting on the marshalled page keeps this test
// compilable against the base commit, where the fields do not exist yet.
func TestDegradedSearchNamesTheRankingPathItUsed(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, reason string
		damage       []string
	}{
		{"empty index", "empty_index", []string{"DELETE FROM search", "DELETE FROM search_rows"}},
		{"index unavailable", "index_unavailable", []string{"DROP TABLE search"}},
	} {
		s := testStore(t)
		create(t, s, "demo/retention-plan.md", "# Retention plan\n\nArchive after thirty days.\n")
		for _, sql := range c.damage {
			if _, e := s.db.Exec(sql); e != nil {
				t.Fatalf("%s: %v", c.name, e)
			}
		}
		p, e := s.Page(ctx, api.Filter{Query: "retention plan", Limit: 20}, true)
		if e != nil {
			t.Fatalf("%s: %v", c.name, e)
		}
		if len(p.Documents) != 1 || p.Documents[0].Path != "demo/retention-plan.md" {
			t.Fatalf("%s: degraded search lost the name match: %+v", c.name, p.Documents)
		}
		b, _ := json.Marshal(p)
		for _, want := range []string{`"ranking":"substring"`, `"ranking_reason":"` + c.reason + `"`} {
			if !strings.Contains(string(b), want) {
				t.Fatalf("%s: page does not report %s: %s", c.name, want, b)
			}
		}
	}
	// The healthy path names itself too, so a reader can tell the two apart.
	s := corpus(t)
	p, e := s.Page(ctx, api.Filter{Query: "agent experience framework", Limit: 20}, true)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"ranking":"bm25"`) {
		t.Fatalf("healthy page does not report its ranking path: %s", b)
	}
}

// A write answers with the new revision; the content the caller just sent does
// not travel back, and is not stored a second time in the receipts table.
func TestWriteReceiptAndStoredReceiptCarryNoContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	body := strings.Repeat("payload line of an audit document\n", 1355)
	if len(body) < 46000 || len(body) > 47000 {
		t.Fatalf("fixture is %d bytes; the measured case is about 46 KB", len(body))
	}
	r, e := s.Mutate(ctx, "host", "big-write", api.Mutation{Operation: "create", Path: "demo/big.md", Content: &body})
	if e != nil {
		t.Fatal(e)
	}
	if r.Document.Content != "" {
		t.Fatalf("write response carried %d bytes of content", len(r.Document.Content))
	}
	if r.Document.Revision != 1 || r.Document.ContentHash != Hash(body) || r.TotalLines != 1355 || r.FullDocumentHash != Hash(body) {
		t.Fatalf("write response lost its useful answer: %+v", r)
	}
	var stored string
	if e = s.db.QueryRow("SELECT response FROM receipts WHERE key='big-write'").Scan(&stored); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(stored, "payload line") {
		t.Fatalf("receipts row stored a second copy of the body: %d bytes", len(stored))
	}
	if len(stored) > 1024 {
		t.Fatalf("receipts row is %d bytes for a one-integer answer", len(stored))
	}
	got, e := s.Get(ctx, "", r.Document.ID, 0)
	if e != nil || got.Document.Content != body {
		t.Fatalf("content is no longer retrievable: %v", e)
	}
	// An idempotent replay answers exactly as the first write did.
	replay, e := s.Mutate(ctx, "host", "big-write", api.Mutation{Operation: "create", Path: "demo/big.md", Content: &body})
	if e != nil || replay.Document != r.Document {
		t.Fatalf("replay differs: %+v %v", replay, e)
	}
}
