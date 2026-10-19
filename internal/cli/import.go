package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/iryzhkov/jocasta/internal/client"
	"golang.org/x/text/unicode/norm"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type ImportEntry struct {
	Source      string `json:"source"`
	Destination string `json:"destination,omitempty"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	Skipped     string `json:"skipped,omitempty"`
}
type Manifest struct {
	Version       string        `json:"version"`
	MigrationID   string        `json:"migration_id"`
	Root          string        `json:"root"`
	Prefix        string        `json:"prefix"`
	InventoriedAt string        `json:"inventoried_at"`
	Entries       []ImportEntry `json:"entries"`
}
type ImportReceipt struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	SHA256      string `json:"sha256"`
	Verified    bool   `json:"verified"`
}

func validImportPath(p string) bool {
	if p == "" || !utf8.ValidString(p) || len(p) > 1024 || strings.Contains(p, "\\") {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") {
			return false
		}
	}
	return true
}
func scan(root, prefix, migrationID string) (Manifest, error) {
	m := Manifest{Version: "jocasta-import/v1", Root: root, Prefix: prefix, MigrationID: migrationID, InventoriedAt: time.Now().UTC().Format(time.RFC3339), Entries: []ImportEntry{}}
	st, e := os.Lstat(root)
	if e != nil {
		return m, localIO(e)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return m, invalid("source root must be a real directory, not a symlink")
	}
	e = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if path == root {
			return walkErr
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		en := ImportEntry{Source: rel, Destination: norm.NFC.String(prefix + "/" + rel)}
		if walkErr != nil {
			en.Skipped = "unreadable"
			m.Entries = append(m.Entries, en)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			en.Skipped = "symlink"
			m.Entries = append(m.Entries, en)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			en.Skipped = "unreadable metadata"
		} else {
			en.Size = st.Size()
			if !st.Mode().IsRegular() {
				en.Skipped = "not a regular file"
			} else if !validImportPath(en.Destination) {
				en.Skipped = "invalid logical destination path"
			} else if st.Size() > MaxDocument {
				en.Skipped = "exceeds 16 MiB limit"
			} else {
				b, e := readContent(path, nil)
				if e != nil {
					en.Skipped = e.Error()
				} else {
					en.SHA256 = digest(b)
				}
			}
		}
		m.Entries = append(m.Entries, en)
		return nil
	})
	if e != nil {
		return m, localIO(e)
	}
	return m, nil
}
func importRoot(pos []string) (string, error) {
	if len(pos) != 1 {
		return "", invalid("import requires one source directory")
	}
	p, e := filepath.Abs(pos[0])
	if e != nil {
		return "", localIO(e)
	}
	// Refuse symlinks in every ancestor, not just at the root.
	resolved, e := filepath.EvalSymlinks(p)
	if e != nil {
		return "", localIO(e)
	}
	if resolved != p {
		return "", invalid("import source must not traverse symlinks")
	}
	return p, nil
}
func inventory(o options, pos []string, out io.Writer) error {
	root, e := importRoot(pos)
	if e != nil {
		return e
	}
	if o.migrationID == "" {
		return invalid("--migration-id is required")
	}
	prefix := norm.NFC.String(strings.TrimSuffix(o.prefix, "/"))
	if prefix == "" {
		prefix = "legacy"
	}
	if !validImportPath(prefix) {
		return invalid("invalid import prefix")
	}
	m, e := scan(root, prefix, o.migrationID)
	if e != nil {
		return e
	}
	if o.manifest != "" {
		abs, e := filepath.Abs(o.manifest)
		if e != nil {
			return localIO(e)
		}
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return invalid("manifest must be stored outside the source directory")
		}
		b, e := json.MarshalIndent(m, "", "  ")
		if e != nil {
			return e
		}
		if e = writeFile(abs, append(b, '\n'), o.overwrite); e != nil {
			return e
		}
	}
	return jsonOut(out, m)
}
func verifyInventory(m Manifest) error {
	now, e := scan(m.Root, m.Prefix, m.MigrationID)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(m.Entries, now.Entries) {
		return &localError{code: ExitConflict, err: fmt.Errorf("source inventory changed; generate and review a fresh manifest before importing")}
	}
	return nil
}
func importManifest(ctx context.Context, c *client.Client, o options, pos []string, out io.Writer) error {
	root, e := importRoot(pos)
	if e != nil {
		return e
	}
	if o.manifest == "" || o.migrationID == "" {
		return invalid("import requires --manifest and --migration-id; run --dry-run first")
	}
	f, e := os.Open(o.manifest)
	if e != nil {
		return localIO(e)
	}
	b, e := io.ReadAll(io.LimitReader(f, 16<<20+1))
	f.Close()
	if e != nil {
		return localIO(e)
	}
	if len(b) > 16<<20 {
		return invalid("manifest exceeds 16 MiB")
	}
	var m Manifest
	if json.Unmarshal(b, &m) != nil || m.Version != "jocasta-import/v1" {
		return invalid("invalid import manifest")
	}
	if m.Root != root || m.MigrationID != o.migrationID || !validImportPath(m.Prefix) {
		return invalid("manifest root, migration ID or prefix mismatch")
	}
	seen := map[string]bool{}
	for _, en := range m.Entries {
		if en.Skipped != "" {
			continue
		}
		if !validImportPath(en.Source) || en.Destination != norm.NFC.String(m.Prefix+"/"+en.Source) || seen[en.Destination] {
			return invalid("invalid or duplicate manifest destination")
		}
		seen[en.Destination] = true
	}
	if e = verifyInventory(m); e != nil {
		return e
	}
	receipts := []ImportReceipt{}
	skipped := []ImportEntry{}
	for _, en := range m.Entries {
		if en.Skipped != "" {
			skipped = append(skipped, en)
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(en.Source))
		st, e := os.Lstat(path)
		if e != nil {
			return localIO(e)
		}
		if !st.Mode().IsRegular() {
			return invalid("source is no longer a regular file")
		}
		content, e := readContent(path, nil)
		if e != nil {
			return e
		}
		if int64(len(content)) != en.Size || digest(content) != en.SHA256 {
			return &localError{code: ExitConflict, err: fmt.Errorf("source changed: %s", en.Source)}
		}
		key := "import-" + digest([]byte(m.MigrationID+"\x00"+en.Source+"\x00"+en.SHA256))
		body := map[string]any{"operation": "create", "path": en.Destination, "content": string(content), "message": "Imported by migration " + m.MigrationID}
		var r Receipt
		if e = c.Call(ctx, http.MethodPost, "/v1/mutate", nil, body, key, &r); e != nil {
			return fmt.Errorf("import %s: %w", en.Source, e)
		}
		var got Receipt
		q := url.Values{"id": {r.Document.ID}, "revision": {fmt.Sprint(r.Document.Revision)}}
		if e = c.Call(ctx, http.MethodGet, "/v1/get", q, nil, "", &got); e != nil {
			return e
		}
		if got.Document.ID == "" || got.Document.Path != en.Destination || got.Document.ContentHash != en.SHA256 || digest([]byte(got.Document.Content)) != en.SHA256 || got.Document.Content != string(content) {
			return &localError{code: ExitConflict, err: fmt.Errorf("destination verification failed for %s", en.Source)}
		}
		receipts = append(receipts, ImportReceipt{en.Source, en.Destination, r.Document.ID, r.Document.Revision, en.SHA256, true})
	}
	if e = verifyInventory(m); e != nil {
		return e
	}
	return jsonOut(out, map[string]any{"version": "jocasta-import/v1", "migration_id": m.MigrationID, "root": m.Root, "verified_at": time.Now().UTC().Format(time.RFC3339), "receipts": receipts, "skipped": skipped, "imported_count": len(receipts), "skipped_count": len(skipped), "sources_preserved": true, "cutover": false})
}
