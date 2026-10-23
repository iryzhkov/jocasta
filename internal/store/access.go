package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/iryzhkov/jocasta/pkg/api"
)

func requireWrite(ctx context.Context) error {
	if access(ctx) != nil {
		return &api.Error{Code: "forbidden", Message: "read-only credential"}
	}
	return nil
}

type accessKey struct{}
type readAccess struct {
	principal string
	projects  []string
}

// WithReadAccess installs immutable, per-token read bounds. An absent context
// retains the local/operator API; an empty or invalid grant never does.
func WithReadAccess(ctx context.Context, principal string, projects []string) (context.Context, error) {
	if principal == "" || len(projects) == 0 || len(projects) > 16 {
		return nil, Invalid("invalid read grants")
	}
	unique := map[string]bool{}
	for _, p := range projects {
		path, err := NormalizePath(p + "/document")
		if err != nil || path != p+"/document" || strings.ContainsAny(p, "/*?") || len(p) > 128 {
			return nil, Invalid("invalid read grants")
		}
		unique[p] = true
	}
	a := &readAccess{principal: principal}
	for p := range unique {
		a.projects = append(a.projects, p)
	}
	sort.Strings(a.projects)
	return context.WithValue(ctx, accessKey{}, a), nil
}
func access(ctx context.Context) *readAccess {
	a, _ := ctx.Value(accessKey{}).(*readAccess)
	return a
}
func allowedPath(ctx context.Context, path string) bool {
	a := access(ctx)
	if a == nil {
		return true
	}
	project, _, ok := strings.Cut(path, "/")
	if !ok {
		return false
	}
	for _, p := range a.projects {
		if p == project {
			return true
		}
	}
	return false
}
func readFilter(ctx context.Context, where string, args []any, column string) (string, []any) {
	a := access(ctx)
	if a == nil {
		return where, args
	}
	marks := make([]string, len(a.projects))
	for i, p := range a.projects {
		marks[i] = "?"
		args = append(args, p)
	}
	return where + " AND substr(" + column + ",1,instr(" + column + ",'/')-1) IN (" + strings.Join(marks, ",") + ")", args
}
func (s *Store) cursorSignature(ctx context.Context, value string) string {
	a := access(ctx)
	if a == nil {
		return Hash(value)
	}
	b, _ := json.Marshal(struct {
		Principal string
		Projects  []string
		Value     string
	}{a.principal, a.projects, value})
	mac := hmac.New(sha256.New, s.cursorKey[:])
	mac.Write(b)
	return hex.EncodeToString(mac.Sum(nil))
}

// Scoped cursors observe only their authorized corpus. Streaming avoids building
// an unbounded snapshot string or exposing global generation changes.
func scopedState(ctx context.Context, tx *sql.Tx) (string, error) {
	where, args := readFilter(ctx, "1=1", nil, "path")
	rows, err := tx.QueryContext(ctx, "SELECT id,revision FROM documents WHERE "+where+" ORDER BY id", args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var id, rev string
		if err = rows.Scan(&id, &rev); err != nil {
			return "", err
		}
		io.WriteString(h, id+":"+rev+";")
	}
	return hex.EncodeToString(h.Sum(nil)), rows.Err()
}
