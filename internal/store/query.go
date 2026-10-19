package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"strconv"
	"strings"

	"github.com/iryzhkov/jocasta/pkg/api"
)

type cursor struct {
	Generation       int64
	Signature, After string
	Maximum          int64
	Offset           int
}

func encode(c cursor) string { b, _ := json.Marshal(c); return base64.RawURLEncoding.EncodeToString(b) }
func decode(raw string) (cursor, error) {
	var c cursor
	if len(raw) > 4096 {
		return c, Invalid("invalid cursor")
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return c, Invalid("invalid cursor")
	}
	if json.Unmarshal(b, &c) != nil {
		return c, Invalid("invalid cursor")
	}
	return c, nil
}
func filterSQL(f api.Filter, search bool) (string, []any) {
	where := "1=1"
	args := []any{}
	if !f.IncludeArchived && !search {
		where += " AND archived=0"
	}
	if f.Prefix != "" {
		where += " AND substr(path,1,?)=?"
		args = append(args, len([]rune(f.Prefix)), f.Prefix)
	}
	if f.Project != "" {
		where += " AND substr(path,1,?)=?"
		args = append(args, len([]rune(f.Project))+1, f.Project+"/")
	}
	if f.Kind != "" {
		where += " AND kind=?"
		args = append(args, f.Kind)
	}
	return where, args
}

// searchSQL builds the one statement that selects and orders search candidates.
//
// Ranking "bm25" asks the full-text index for a relevance score and adds it to
// the document's own name score, so a document that answers the query with its
// own name and body outranks one that merely mentions it. The score adds the
// whole-query name tiers (equality, prefix, substring) to per-token path and
// title matches, which is what lets a multi-word query reach a hyphenated path
// or a derived title, and finally the index's own relevance. Equal scores are
// ordered by recency and then by path, which is unique, and never by the random
// ID.
//
// The two candidate sources are unioned and then looked up in documents by
// primary key, rather than the index being joined onto a scan of documents. A
// CTE carries no index, so "documents LEFT JOIN fts ON fts_id=id" makes SQLite
// rescan the whole materialised index result once per document row. That is
// quadratic in the number of documents, and the query plan hides it as a single
// linear-looking "SCAN fts LEFT-JOIN" line. Reading the index's rows as
// candidates and joining documents on its primary key keeps the statement
// linearithmic, which is what the base implementation's "id IN (subquery)" got
// from the ephemeral index SQLite builds for an IN list.
//
// Ranking "substring" is the same statement without the index: no content
// matching and no relevance term. It is what search falls back to when the
// projection cannot answer the query at all.
func searchSQL(ranking string, f api.Filter, where string, filterArgs []any, q string, fuzzy []string, limit, offset int) (string, []any) {
	names := "(instr(lower(path),?)>0 OR instr(lower(title),?)>0 OR id IN (SELECT id FROM paths WHERE instr(lower(path),?)>0))"
	nameArgs := []any{q, q, q}
	if len(fuzzy) > 0 {
		placeholders := make([]string, len(fuzzy))
		for i, id := range fuzzy {
			placeholders[i] = "?"
			nameArgs = append(nameArgs, id)
		}
		names += " OR id IN (" + strings.Join(placeholders, ",") + ")"
	}
	score := "CASE WHEN lower(path)=? OR lower(title)=? THEN 1000 ELSE 0 END" +
		"+CASE WHEN substr(lower(path),1,?)=? OR substr(lower(title),1,?)=? THEN 500 ELSE 0 END" +
		"+CASE WHEN instr(lower(path),?)>0 OR instr(lower(title),?)>0 THEN 250 ELSE 0 END"
	scoreArgs := []any{q, q, len([]rune(q)), q, len([]rune(q)), q, q, q}
	for _, token := range searchTokens(q) {
		score += "+CASE WHEN instr(lower(path),?)>0 THEN 40 ELSE 0 END+CASE WHEN instr(lower(title),?)>0 THEN 30 ELSE 0 END"
		scoreArgs = append(scoreArgs, token, token)
	}
	if ranking != "bm25" {
		args := append([]any{}, filterArgs...)
		args = append(args, nameArgs...)
		args = append(args, scoreArgs...)
		args = append(args, limit+1, offset)
		return "SELECT id,path,title,archived FROM documents WHERE " + where + " AND (" + names + ")" +
			" ORDER BY (" + score + ") DESC,updated_at DESC,path LIMIT ? OFFSET ?", args
	}
	// The projected path and title weigh more than the body, so a document the
	// query names beats one that only contains the same words. The index is the
	// first placeholder in the statement, so its argument goes first.
	//
	// A candidate carries a null relevance when only its name matched, so max()
	// over the two sources is the relevance of the document and not of one of
	// its two rows; named records which source it came from, because a content
	// match of an archived document is excluded where a name match is not.
	archived := " AND (named=1 OR archived=0)"
	if f.IncludeArchived {
		archived = ""
	}
	statement := "WITH fts(fts_id,fts_score) AS (SELECT search_rows.id,-bm25(search,0.0,8.0,6.0,1.0) FROM search JOIN search_rows ON search_rows.rowid=search.rowid WHERE search MATCH ?)" +
		",candidates(candidate_id,candidate_score,candidate_named) AS (SELECT id,NULL,1 FROM documents WHERE " + names +
		" UNION ALL SELECT fts_id,fts_score,0 FROM fts)" +
		",ranked(ranked_id,relevance,named) AS (SELECT candidate_id,max(candidate_score),max(candidate_named) FROM candidates GROUP BY candidate_id) " +
		"SELECT id,path,title,archived FROM ranked JOIN documents ON documents.id=ranked.ranked_id WHERE " + where + archived +
		" ORDER BY (" + score + "+COALESCE(relevance,0)*3) DESC,updated_at DESC,path LIMIT ? OFFSET ?"
	args := []any{matchExpression(f.Query)}
	args = append(args, nameArgs...)
	args = append(args, filterArgs...)
	args = append(args, scoreArgs...)
	args = append(args, limit+1, offset)
	return statement, args
}

// searchTokens returns at most eight lowercased query words, which bounds the
// per-token part of the score for a long query.
func searchTokens(q string) []string {
	tokens := strings.Fields(q)
	if len(tokens) > 8 {
		tokens = tokens[:8]
	}
	return tokens
}

// matchExpression turns user input into an FTS5 phrase query. Every word is
// quoted, so the input is never read as FTS operator syntax.
func matchExpression(query string) string {
	tokens := strings.Fields(query)
	for i, token := range tokens {
		tokens[i] = "\"" + strings.ReplaceAll(token, "\"", "\"\"") + "\""
	}
	return strings.Join(tokens, " AND ")
}

// indexState names why the full-text projection cannot rank a query, or returns
// the empty string when it can.
func indexState(ctx context.Context, q querier) (string, error) {
	var present int
	if e := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM search_rows)").Scan(&present); e != nil {
		if indexUnavailable(e) {
			return "index_unavailable", nil
		}
		return "", e
	}
	if present == 0 {
		return "empty_index", nil
	}
	return "", nil
}

// indexUnavailable distinguishes a full-text projection that cannot answer --
// dropped, not yet built, or an SQLite build without FTS5 -- and a query the
// FTS parser rejects, from a real database error, which is still returned.
func indexUnavailable(err error) bool {
	m := strings.ToLower(err.Error())
	for _, symptom := range []string{"fts5", "no such table: search", "no such table: search_rows", "no such function: bm25", "no such module"} {
		if strings.Contains(m, symptom) {
			return true
		}
	}
	return false
}
func rejectionReason(err error) string {
	if strings.Contains(strings.ToLower(err.Error()), "syntax") {
		return "query_rejected"
	}
	return "index_unavailable"
}
func (s *Store) Page(ctx context.Context, f api.Filter, search bool) (api.Page, error) {
	p := api.Page{Version: api.Version, Documents: []api.Document{}, Complete: true}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit < 1 || f.Limit > 200 {
		return p, Invalid("limit must be 1..200")
	}
	if len(f.Query) > 1024 {
		return p, Invalid("query exceeds 1024 bytes")
	}
	if search && strings.TrimSpace(f.Query) == "" {
		return p, Invalid("search query is required")
	}
	if f.Mode != "" && f.Mode != "default" && f.Mode != "literal" {
		return p, Invalid("search mode must be default or literal")
	}
	rawCursor := f.Cursor
	f.Cursor = ""
	signatureBytes, _ := json.Marshal(struct {
		Filter api.Filter
		Search bool
	}{f, search})
	signature := Hash(string(signatureBytes))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var generation int64
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM state").Scan(&generation); err != nil {
		return p, err
	}
	c := cursor{Generation: generation, Signature: signature}
	if rawCursor != "" {
		c, err = decode(rawCursor)
		if err != nil {
			return p, err
		}
		if c.Signature != signature || c.Offset < 0 {
			return p, Invalid("cursor filters do not match")
		}
		if c.Generation != generation {
			return p, &api.Error{Code: "cursor_expired", Message: "documents changed; restart enumeration"}
		}
	}
	where, args := filterSQL(f, search)
	q := strings.ToLower(f.Query)
	defaultSearch := search && f.Mode != "literal"
	budget := f.Limit
	if search {
		budget = 500
		p.CandidateLimit = 500
	}
	var rows *sql.Rows
	if defaultSearch {
		// Fuzzy work is bounded metadata-only expansion. Exact names and FTS candidates
		// are selected globally, so a known document never depends on its random ID.
		fuzzyRows, e := tx.QueryContext(ctx, "SELECT id,path,title FROM documents WHERE "+where+" ORDER BY id LIMIT 501", args...)
		if e != nil {
			return p, e
		}
		fuzzy := []string{}
		n := 0
		for fuzzyRows.Next() {
			var id, path, title string
			if e = fuzzyRows.Scan(&id, &path, &title); e != nil {
				fuzzyRows.Close()
				return p, e
			}
			n++
			if n <= 500 && (nearName(q, strings.ToLower(path)) || nearName(q, strings.ToLower(title))) {
				fuzzy = append(fuzzy, id)
			}
		}
		e = fuzzyRows.Err()
		fuzzyRows.Close()
		if e != nil {
			return p, e
		}
		if n > 500 {
			p.Complete = false
		}
		p.Ranking = "bm25"
		if p.RankingReason, e = indexState(ctx, tx); e != nil {
			return p, e
		} else if p.RankingReason != "" {
			p.Ranking = "substring"
		}
		query, queryArgs := searchSQL(p.Ranking, f, where, args, q, fuzzy, budget, c.Offset)
		rows, err = tx.QueryContext(ctx, query, queryArgs...)
		if err != nil && p.Ranking == "bm25" && indexUnavailable(err) {
			// The projection could not answer this query. Say which path
			// ordered the page rather than returning an arbitrary order.
			p.Ranking, p.RankingReason = "substring", rejectionReason(err)
			query, queryArgs = searchSQL(p.Ranking, f, where, args, q, fuzzy, budget, c.Offset)
			rows, err = tx.QueryContext(ctx, query, queryArgs...)
		}
		if err != nil {
			return p, err
		}
	} else {
		if search {
			// A literal scan is not a ranking; it walks documents in ID order
			// because that is the total order its cursor pages through.
			p.Ranking = "literal"
		}
		where += " AND id>?"
		args = append(args, c.After, budget+1)
		rows, err = tx.QueryContext(ctx, "SELECT id,path,title,archived FROM documents WHERE "+where+" ORDER BY id LIMIT ?", args...)
		if err != nil {
			return p, err
		}
	}
	type candidate struct {
		id, path, title string
		archived        bool
	}
	var candidates []candidate
	for rows.Next() {
		var x candidate
		if err = rows.Scan(&x.id, &x.path, &x.title, &x.archived); err != nil {
			rows.Close()
			return p, err
		}
		candidates = append(candidates, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	more := len(candidates) > budget
	if more {
		candidates = candidates[:budget]
	}
	last := c.After
	scannedBytes := 0
	for _, x := range candidates {
		if len(p.Documents) >= f.Limit || scannedBytes >= 32<<20 {
			more = true
			break
		}
		last = x.id
		p.ScannedCount++
		matched := !search || defaultSearch
		var doc api.Document
		if !search {
			var raw string
			if err = tx.QueryRowContext(ctx, "SELECT json_remove(snapshot,'$.content') FROM documents WHERE id=?", x.id).Scan(&raw); err != nil {
				return p, err
			}
			if err = json.Unmarshal([]byte(raw), &doc); err != nil {
				return p, err
			}
		} else {
			doc, _, err = resolve(ctx, tx, "", x.id, 0)
			if err != nil {
				return p, err
			}
			scannedBytes += len(doc.Content)
			if !defaultSearch {
				matched = strings.Contains(strings.ToLower(x.path), q) || strings.Contains(strings.ToLower(x.title), q)
				if !matched {
					var count int
					if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM paths WHERE id=? AND instr(lower(path),?)>0", x.id, q).Scan(&count); err != nil {
						return p, err
					}
					matched = count > 0
				}
				if !x.archived || f.IncludeArchived {
					matched = matched || strings.Contains(strings.ToLower(doc.Content), q)
				}
			}
			doc.Content = excerpt(doc.Content, q)
		}
		// Titles are derived on write and indexed; a result carries the title a
		// reader would recognise rather than the empty string an unset title
		// leaves in the snapshot.
		doc.Title = x.title
		if matched {
			p.Documents = append(p.Documents, doc)
		}
	}
	p.HasMore = more
	p.Complete = p.Complete && !more
	if more {
		c.After = last
		c.Offset += p.ScannedCount
		p.NextCursor = encode(c)
	}
	return p, nil
}
func nearName(q, name string) bool {
	if len([]rune(q)) < 4 || len([]rune(q)) > 128 {
		return false
	}
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '.' || r == '-' || r == '_' || r == ' ' })
	words = append(words, name)
	for _, w := range words {
		if distanceOne([]rune(q), []rune(w)) {
			return true
		}
	}
	return false
}
func distanceOne(a, b []rune) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if len(a) == len(b) {
			i++
		}
		j++
	}
	return edits+len(a)-i+len(b)-j <= 1
}
func excerpt(content, q string) string {
	r := []rune(content)
	if len(r) <= 320 {
		return content
	}
	lower := strings.ToLower(content)
	at := strings.Index(lower, q)
	start := 0
	if at > 0 {
		start = len([]rune(lower[:at])) - 80
		if start < 0 {
			start = 0
		}
	}
	end := start + 320
	if end > len(r) {
		end = len(r)
	}
	return string(r[start:end])
}
func (s *Store) History(ctx context.Context, path, id string, limit int, rawCursor string) (api.Page, error) {
	p := api.Page{Version: api.Version, Documents: []api.Document{}, Complete: true}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return p, Invalid("limit must be 1..200")
	}
	d, _, err := resolve(ctx, s.db, path, id, 0)
	if err != nil {
		return p, err
	}
	signature := Hash(fmt.Sprintf("history:%s:%d", d.ID, limit))
	c := cursor{Signature: signature, Maximum: d.Revision}
	if rawCursor != "" {
		c, err = decode(rawCursor)
		if err != nil {
			return p, err
		}
		if c.Signature != signature || c.Maximum < 1 {
			return p, Invalid("history cursor does not match")
		}
	}
	after := int64(0)
	if c.After != "" {
		after, err = strconv.ParseInt(c.After, 10, 64)
		if err != nil {
			return p, Invalid("invalid history cursor")
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT json_remove(snapshot,'$.content') FROM revisions WHERE id=? AND revision>? AND revision<=? ORDER BY revision LIMIT ?", d.ID, after, c.Maximum, limit+1)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return p, err
		}
		var snapshot api.Document
		if err = json.Unmarshal([]byte(raw), &snapshot); err != nil {
			return p, err
		}
		snapshot.Content = ""
		p.Documents = append(p.Documents, snapshot)
	}
	if err = rows.Err(); err != nil {
		return p, err
	}
	if len(p.Documents) > limit {
		p.Documents = p.Documents[:limit]
		p.HasMore = true
		p.Complete = false
		c.After = strconv.FormatInt(p.Documents[len(p.Documents)-1].Revision, 10)
		p.NextCursor = encode(c)
	}
	return p, nil
}
func Range(r api.Receipt, lines string) (api.Receipt, error) {
	a, b, ok := strings.Cut(lines, ":")
	if !ok {
		return r, Invalid("lines must be START:END")
	}
	start, e1 := strconv.Atoi(a)
	end, e2 := strconv.Atoi(b)
	if e1 != nil || e2 != nil || start < 1 || end < start || start > r.TotalLines {
		return r, Invalid("line range must be positive, ordered and start within the document")
	}
	actual := end
	if actual > r.TotalLines {
		actual = r.TotalLines
	}
	content := r.Document.Content
	line, begin, finish := 1, 0, len(content)
	for i := 0; i < len(content); i++ {
		if content[i] != '\n' {
			continue
		}
		if line == actual {
			finish = i + 1
			break
		}
		line++
		if line == start {
			begin = i + 1
		}
	}
	r.Document.Content = content[begin:finish]
	r.Partial = true
	r.RequestedLines = lines
	r.ReturnedLines = fmt.Sprintf("%d:%d", start, actual)
	return r, nil
}
