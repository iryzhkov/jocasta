package store

import (
	"context"
	"strings"
	"testing"

	"github.com/iryzhkov/jocasta/pkg/api"
)

// The search statement must never make the full-text result an inner loop of a
// scan of documents. A CTE carries no index, so "documents LEFT JOIN fts" costs
// one rescan of the whole index result per document row -- quadratic work that
// the query plan reports as a single line, "SCAN fts LEFT-JOIN", which reads
// like any other linear scan. That shape made a search of a 3,000 document
// corpus take 626 ms where the same corpus answered in 21 ms before the ranking
// changed. Asserting on the plan pins the shape, because the cost is invisible
// in the SQL text and only becomes visible once the corpus is large.
func TestSearchStatementDoesNotRescanTheIndexPerDocument(t *testing.T) {
	s := corpus(t)
	const query = "agent experience framework"
	statement, args := searchSQL("bm25", api.Filter{Query: query}, "1=1", nil, query, nil, 500, 0)
	rows, e := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+statement, args...)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if e := rows.Scan(&id, &parent, &unused, &detail); e != nil {
			t.Fatal(e)
		}
		plan = append(plan, detail)
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(plan, "\n")
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN fts") {
			t.Fatalf("the full-text result is scanned as a join operand:\n%s", joined)
		}
	}
	if !strings.Contains(joined, "SEARCH documents") {
		t.Fatalf("documents is not reached through its primary key:\n%s", joined)
	}
}
