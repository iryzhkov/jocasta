package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/jocasta/pkg/api"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

type Config struct {
	Path                                            string
	MaxDocumentBytes, MaxStorageBytes, MinFreeBytes int64
}
type Store struct {
	db        *sql.DB
	cfg       Config
	mu        sync.Mutex
	cursorKey [32]byte
}

func Invalid(message string) error { return &api.Error{Code: "invalid_input", Message: message} }
func Open(cfg Config) (*Store, error) {
	if cfg.MaxDocumentBytes == 0 {
		cfg.MaxDocumentBytes = 16 << 20
	}
	if cfg.MaxStorageBytes == 0 {
		cfg.MaxStorageBytes = 10 << 30
	}
	if cfg.MinFreeBytes == 0 {
		cfg.MinFreeBytes = 1 << 30
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	file.Close()
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, cfg: cfg}
	if _, err = rand.Read(s.cursorKey[:]); err != nil {
		db.Close()
		return nil, err
	}
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > 2 {
		return fail(fmt.Errorf("unsupported database schema %d; binary supports 2", version))
	}
	if _, err = db.Exec("PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;"); err != nil {
		return fail(err)
	}
	if version == 0 {
		_, err = db.Exec(`BEGIN IMMEDIATE;
 CREATE TABLE IF NOT EXISTS documents(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, path TEXT NOT NULL UNIQUE, title TEXT NOT NULL, kind TEXT NOT NULL, archived INTEGER NOT NULL, updated_at TEXT NOT NULL, snapshot TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS revisions(id TEXT NOT NULL, revision INTEGER NOT NULL, snapshot TEXT NOT NULL, PRIMARY KEY(id,revision));
 CREATE TABLE IF NOT EXISTS paths(path TEXT PRIMARY KEY,id TEXT NOT NULL REFERENCES documents(id));
 CREATE TABLE IF NOT EXISTS receipts(actor TEXT NOT NULL,key TEXT NOT NULL,digest TEXT NOT NULL,response TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(actor,key));
 CREATE TABLE IF NOT EXISTS activity(id TEXT PRIMARY KEY, last_active TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS state(singleton INTEGER PRIMARY KEY CHECK(singleton=1),generation INTEGER NOT NULL);
 INSERT OR IGNORE INTO state VALUES(1,0);
 CREATE VIRTUAL TABLE IF NOT EXISTS search USING fts5(id UNINDEXED,path,title,content,tokenize='unicode61');
 CREATE TABLE search_rows(rowid INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE);
 PRAGMA user_version=2; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version == 1 {
		// Backup the authoritative database through SQLite before changing its schema.
		backup := cfg.Path + ".pre-v2." + time.Now().UTC().Format("20060102T150405.000000000") + ".db"
		usage, e := s.DiskUsage()
		if e != nil {
			return fail(e)
		}
		if usage.FreeBytes-usage.DatabaseBytes-usage.WALBytes < cfg.MinFreeBytes {
			return fail(&api.Error{Code: "storage_full", Message: "insufficient migration backup headroom"})
		}
		if e = s.Backup(context.Background(), backup); e != nil {
			return fail(fmt.Errorf("pre-migration backup: %w", e))
		}
		if _, e = db.Exec("BEGIN IMMEDIATE; CREATE TABLE search_rows(rowid INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE); INSERT INTO search_rows SELECT rowid,id FROM search; PRAGMA user_version=2; COMMIT;"); e != nil {
			return fail(fmt.Errorf("schema 2 migration failed; backup %s: %w", backup, e))
		}
	}
	if _, err = db.Exec("CREATE TABLE IF NOT EXISTS activity(id TEXT PRIMARY KEY,last_active TEXT NOT NULL); INSERT OR IGNORE INTO activity SELECT id,strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM documents;"); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func NormalizePath(path string) (string, error) {
	path = norm.NFC.String(path)
	if !utf8.ValidString(path) || len(path) > 1024 || !strings.Contains(path, "/") {
		return "", Invalid("path must be valid UTF-8, at most 1024 bytes and include a project namespace")
	}
	for _, r := range path {
		if unicode.IsControl(r) || r == '\\' {
			return "", Invalid("path cannot contain controls or backslashes")
		}
	}
	for i, p := range strings.Split(path, "/") {
		if p == "" || p == "." || p == ".." || (i == 0 && strings.HasPrefix(p, "_")) {
			return "", Invalid("path contains an empty, traversal or reserved segment")
		}
	}
	return path, nil
}
func Hash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}
func Title(d api.Document) string {
	if d.Title != "" {
		return d.Title
	}
	c := d.Content
	if len(c) > 65536 {
		c = c[:65536]
	}
	for _, line := range strings.Split(c, "\n") {
		if strings.HasPrefix(line, "#") {
			line = strings.TrimSpace(strings.TrimLeft(line, "#"))
			if line != "" {
				r := []rune(line)
				if len(r) > 256 {
					r = r[:256]
				}
				return string(r)
			}
		}
	}
	p := strings.Split(d.Path, "/")
	r := []rune(p[len(p)-1])
	if len(r) > 256 {
		r = r[:256]
	}
	return string(r)
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func resolve(ctx context.Context, q querier, path, id string, revision int64) (api.Document, bool, error) {
	var d api.Document
	if (path == "") == (id == "") {
		return d, false, Invalid("supply exactly one path or id")
	}
	if path != "" {
		var err error
		path, err = NormalizePath(path)
		if err != nil {
			return d, false, err
		}
		if !allowedPath(ctx, path) {
			return d, false, notFound(sql.ErrNoRows)
		}
		err = q.QueryRowContext(ctx, "SELECT id FROM paths WHERE path=?", path).Scan(&id)
		if err != nil {
			return d, false, notFound(err)
		}
	}
	if access(ctx) != nil {
		var current string
		if err := q.QueryRowContext(ctx, "SELECT path FROM documents WHERE id=?", id).Scan(&current); err != nil {
			return d, false, notFound(err)
		}
		if !allowedPath(ctx, current) {
			return d, false, notFound(sql.ErrNoRows)
		}
	}
	query := "SELECT snapshot FROM documents WHERE id=?"
	args := []any{id}
	if revision > 0 {
		query = "SELECT snapshot FROM revisions WHERE id=? AND revision=?"
		args = append(args, revision)
	}
	var raw string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		return d, false, notFound(err)
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return d, false, err
	}
	if !allowedPath(ctx, d.Path) {
		return api.Document{}, false, notFound(sql.ErrNoRows)
	}
	return d, path != "" && path != d.Path, nil
}
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &api.Error{Code: "not_found", Message: "document or revision does not exist"}
	}
	return err
}
func (s *Store) Get(ctx context.Context, path, id string, revision int64) (api.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision < 0 {
		return api.Receipt{}, Invalid("revision must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Receipt{}, err
	}
	defer tx.Rollback()
	d, alias, err := resolve(ctx, tx, path, id, revision)
	if err != nil {
		return api.Receipt{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO activity VALUES(?,?) ON CONFLICT(id) DO UPDATE SET last_active=excluded.last_active", d.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return api.Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.Receipt{}, err
	}
	return receipt(d, path, id, alias), nil
}
func receipt(d api.Document, path, id string, alias bool) api.Receipt {
	ref := path
	if ref == "" {
		ref = id
	}
	lines := strings.Count(d.Content, "\n")
	if d.Content != "" && !strings.HasSuffix(d.Content, "\n") {
		lines++
	}
	return api.Receipt{Version: api.Version, Document: d, RequestedReference: ref, Alias: alias, FullDocumentHash: d.ContentHash, TotalLines: lines}
}
func (s *Store) checkBudget(extra int64) error {
	var size int64
	for _, p := range []string{s.cfg.Path, s.cfg.Path + "-wal", s.cfg.Path + "-shm"} {
		if st, err := os.Stat(p); err == nil {
			size += st.Size()
		}
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(s.cfg.Path), &fs); err != nil {
		return err
	}
	if size+extra > s.cfg.MaxStorageBytes || int64(fs.Bavail)*int64(fs.Bsize)-extra < s.cfg.MinFreeBytes {
		return &api.Error{Code: "storage_full", Message: "storage budget or reserved disk headroom reached"}
	}
	return nil
}
func (s *Store) Mutate(ctx context.Context, actor, key string, m api.Mutation) (api.Receipt, error) {
	var empty api.Receipt
	if access(ctx) != nil {
		return empty, &api.Error{Code: "forbidden", Message: "read-only credential"}
	}
	if actor == "" || key == "" || len(key) > 256 {
		return empty, Invalid("idempotency key is required and must be at most 256 bytes")
	}
	if len(m.Message) > 4096 {
		return empty, Invalid("message exceeds 4096 bytes")
	}
	if m.Content != nil && (int64(len(*m.Content)) > s.cfg.MaxDocumentBytes || !utf8.ValidString(*m.Content) || strings.ContainsRune(*m.Content, 0)) {
		return empty, Invalid("content must be valid UTF-8 without NUL and within document byte limit")
	}
	for _, p := range []*string{m.Title, m.Kind} {
		if p != nil && (len(*p) > 1024 || !utf8.ValidString(*p) || strings.ContainsRune(*p, 0)) {
			return empty, Invalid("title/kind must be valid UTF-8 without NUL and at most 1024 bytes")
		}
	}
	raw, _ := json.Marshal(m)
	digest := Hash(string(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	// Acquire SQLite's write lock before reading the current revision.
	if _, err = tx.ExecContext(ctx, "UPDATE state SET generation=generation WHERE singleton=1"); err != nil {
		return empty, err
	}
	var priorDigest, prior string
	err = tx.QueryRowContext(ctx, "SELECT digest,response FROM receipts WHERE actor=? AND key=?", actor, key).Scan(&priorDigest, &prior)
	if err == nil {
		if priorDigest != digest {
			return empty, &api.Error{Code: "idempotency_conflict", Message: "key was already used with different input"}
		}
		var r api.Receipt
		err = json.Unmarshal([]byte(prior), &r)
		return r, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	var d api.Document
	var alias bool
	changed := true
	switch m.Operation {
	case "create":
		if m.ID != "" || m.IfRevision != 0 || m.Content == nil {
			return empty, Invalid("create requires path/content and no id or if_revision")
		}
		d.Path, err = NormalizePath(m.Path)
		if err != nil {
			return empty, err
		}
		var exists string
		err = tx.QueryRowContext(ctx, "SELECT id FROM paths WHERE path=?", d.Path).Scan(&exists)
		if err == nil {
			return empty, &api.Error{Code: "conflict", Message: "path or alias already reserved", ID: exists}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return empty, err
		}
		d.ID = hex.EncodeToString(b)
		d.Content = *m.Content
	default:
		d, alias, err = resolve(ctx, tx, m.Path, m.ID, 0)
		if err != nil {
			return empty, err
		}
		if m.IfRevision <= 0 {
			return empty, Invalid("mutation requires a positive if_revision")
		}
		if d.Revision != m.IfRevision {
			// Mutate stamps the actor on every write, so the writer of the
			// current revision is in hand here and travels with the refusal.
			return empty, &api.Error{Code: "conflict", Message: "document changed; reread and reconcile", ExpectedRevision: m.IfRevision, CurrentRevision: d.Revision, ID: d.ID, Actor: d.Actor}
		}
		old := d
		switch m.Operation {
		case "put":
			if m.Content != nil {
				d.Content = *m.Content
			}
		case "move":
			d.Path, err = NormalizePath(m.Destination)
			if err != nil {
				return empty, err
			}
		case "archive":
			d.Archived = true
		case "unarchive":
			d.Archived = false
		case "restore":
			if m.SourceRevision <= 0 {
				return empty, Invalid("restore requires positive source_revision")
			}
			source, _, e := resolve(ctx, tx, "", d.ID, m.SourceRevision)
			if e != nil {
				return empty, e
			}
			d.Content = source.Content
			d.Title = source.Title
			d.Kind = source.Kind
		default:
			return empty, Invalid("unknown mutation operation")
		}
		if m.Title != nil {
			d.Title = *m.Title
		}
		if m.Kind != nil {
			d.Kind = *m.Kind
		}
		changed = d.Content != old.Content || d.Title != old.Title || d.Kind != old.Kind || d.Path != old.Path || d.Archived != old.Archived
	}
	if m.Operation == "create" {
		if m.Title != nil {
			d.Title = *m.Title
		}
		if m.Kind != nil {
			d.Kind = *m.Kind
		}
	}
	if changed {
		encodedSnapshot, _ := json.Marshal(d)
		if err = s.checkBudget(int64(len(encodedSnapshot))*12 + 65536); err != nil {
			return empty, err
		}
		var owner string
		err = tx.QueryRowContext(ctx, "SELECT id FROM paths WHERE path=?", d.Path).Scan(&owner)
		if err == nil && owner != d.ID {
			return empty, &api.Error{Code: "conflict", Message: "destination path or alias already reserved", ID: owner}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
		d.Revision++
		d.Actor = actor
		d.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		d.ContentHash = Hash(d.Content)
		d.Message = m.Message
		snapshot, _ := json.Marshal(d)
		if _, err = tx.ExecContext(ctx, "INSERT INTO documents VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,path=excluded.path,title=excluded.title,kind=excluded.kind,archived=excluded.archived,updated_at=excluded.updated_at,snapshot=excluded.snapshot", d.ID, d.Revision, d.Path, Title(d), d.Kind, d.Archived, d.UpdatedAt, string(snapshot)); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO paths VALUES(?,?)", d.Path, d.ID); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO revisions VALUES(?,?,?)", d.ID, d.Revision, string(snapshot)); err != nil {
			return empty, err
		}
		// A new document cannot have a projection. FTS5's UNINDEXED id is not
		// indexed; deleting on create would scan the growing corpus on every import.
		if m.Operation != "create" {
			if _, err = tx.ExecContext(ctx, "DELETE FROM search WHERE rowid=(SELECT rowid FROM search_rows WHERE id=?)", d.ID); err != nil {
				return empty, err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM search_rows WHERE id=?", d.ID); err != nil {
				return empty, err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO search(id,path,title,content) VALUES(?,?,?,?)", d.ID, d.Path, Title(d), d.Content); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO search_rows(rowid,id) VALUES(last_insert_rowid(),?)", d.ID); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE state SET generation=generation+1 WHERE singleton=1"); err != nil {
			return empty, err
		}
	}
	result := receipt(d, m.Path, m.ID, alias)
	// The useful answer of a write is the new revision. The content the caller
	// just sent does not travel back over the wire, and is not stored a second
	// time in the receipts table; the hash, the line count and the revision are
	// what a caller needs to verify the write, and get returns the bytes.
	result.Document.Content = ""
	response, _ := json.Marshal(result)
	if err = s.checkBudget(int64(len(response))*2 + 16384); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO receipts VALUES(?,?,?,?,?)", actor, key, digest, string(response), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO activity VALUES(?,?) ON CONFLICT(id) DO UPDATE SET last_active=excluded.last_active", d.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return result, nil
}
func (s *Store) Backup(ctx context.Context, destination string) error {
	if err := requireWrite(ctx); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("backup destination exists")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destination)
	if err == nil {
		err = os.Chmod(destination, 0600)
	}
	return err
}
func (s *Store) Reindex(ctx context.Context) error {
	if err := requireWrite(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM search_rows"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM search"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO search(id,path,title,content) SELECT id,path,title,json_extract(snapshot,'$.content') FROM documents"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO search_rows SELECT rowid,id FROM search"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE state SET generation=generation+1 WHERE singleton=1")
	if err != nil {
		return err
	}
	return tx.Commit()
}

type DiskUsage struct{ DatabaseBytes, WALBytes, FreeBytes int64 }

func (s *Store) DiskUsage() (DiskUsage, error) {
	var d DiskUsage
	if st, err := os.Stat(s.cfg.Path); err == nil {
		d.DatabaseBytes = st.Size()
	}
	if st, err := os.Stat(s.cfg.Path + "-wal"); err == nil {
		d.WALBytes = st.Size()
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(s.cfg.Path), &fs); err != nil {
		return d, err
	}
	d.FreeBytes = int64(fs.Bavail) * int64(fs.Bsize)
	return d, nil
}
func (s *Store) Health(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT generation FROM state").Scan(&n); err != nil {
		return err
	}
	return s.checkBudget(0)
}
