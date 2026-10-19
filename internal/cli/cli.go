// Package cli provides the stable Jocasta command interface.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iryzhkov/jocasta/internal/buildinfo"
	"github.com/iryzhkov/jocasta/internal/client"
	"github.com/iryzhkov/jocasta/pkg/api"
)

const MaxDocument = 16 << 20
const (
	ExitOK          = 0
	ExitNotFound    = 2
	ExitConflict    = 3
	ExitAuth        = 4
	ExitUnavailable = 5
	ExitInvalid     = 6
	ExitIO          = 7
)

type Document = api.Document
type Receipt = api.Receipt
type Page = api.Page
type localError struct {
	code int
	err  error
	// silent means the command already reported its outcome on stdout and only
	// the exit code remains to be delivered; nothing more is printed.
	silent bool
}

func (e *localError) Error() string { return e.err.Error() }
func invalid(s string) error        { return &localError{code: ExitInvalid, err: errors.New(s)} }
func localIO(e error) error         { return &localError{code: ExitIO, err: e} }
func digest(b []byte) string        { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func jsonOut(w io.Writer, v any) error {
	if e := json.NewEncoder(w).Encode(v); e != nil {
		return localIO(e)
	}
	return nil
}

type options struct {
	titleSet, kindSet                                                                                                      bool
	config, id, file, output, lines, message, title, kind, project, cursor, mode, requestID, manifest, migrationID, prefix string
	json, create, overwrite, includeArchived, dryRun, verbose                                                              bool
	revision, ifRevision, from, to                                                                                         int64
	limit                                                                                                                  int
}

// newFlagSet registers every flag the command interface accepts. There is one
// global flagset rather than one per verb, because the parser must classify a
// token as a flag or a positional before a verb is chosen. The help pages
// render each flag's description and default from this same flagset, so a page
// cannot describe a flag the CLI does not accept or a default it does not use.
func newFlagSet(o *options) *flag.FlagSet {
	h, _ := os.UserHomeDir()
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(h, ".config")
	}
	fs := flag.NewFlagSet("jocasta", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.config, "config", filepath.Join(base, "jocasta", "config.json"), "configuration file")
	fs.StringVar(&o.id, "id", "", "document ID")
	fs.StringVar(&o.file, "file", "", "input file, or -")
	fs.StringVar(&o.output, "output", "", "output file")
	fs.StringVar(&o.lines, "lines", "", "inclusive line range")
	fs.StringVar(&o.message, "message", "", "change message")
	fs.StringVar(&o.title, "title", "", "explicit title")
	fs.StringVar(&o.kind, "kind", "", "kind")
	fs.StringVar(&o.project, "project", "", "project filter")
	fs.StringVar(&o.prefix, "prefix", "", "path prefix")
	fs.StringVar(&o.cursor, "cursor", "", "opaque continuation cursor")
	fs.StringVar(&o.mode, "mode", "default", "search mode")
	fs.StringVar(&o.requestID, "request-id", "", "idempotency key")
	fs.StringVar(&o.manifest, "manifest", "", "import manifest")
	fs.StringVar(&o.migrationID, "migration-id", "", "stable migration ID")
	fs.BoolVar(&o.json, "json", false, "JSON output")
	fs.BoolVar(&o.create, "create", false, "create document")
	fs.BoolVar(&o.overwrite, "overwrite", false, "replace output file")
	fs.BoolVar(&o.includeArchived, "include-archived", false, "include archived")
	fs.BoolVar(&o.dryRun, "dry-run", false, "inventory only")
	fs.BoolVar(&o.verbose, "verbose", false, "on a write, read the revision back and report the whole document")
	fs.Int64Var(&o.revision, "revision", 0, "historical revision")
	fs.Int64Var(&o.ifRevision, "if-revision", 0, "expected current revision")
	fs.Int64Var(&o.from, "from", 0, "first revision")
	fs.Int64Var(&o.to, "to", 0, "second revision")
	fs.IntVar(&o.limit, "limit", 50, "page limit")
	return fs
}

func parse(args []string) (string, options, []string, error) {
	var o options
	fs := newFlagSet(&o)
	// Accept flags before or after positional arguments without interpreting values as commands.
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			f := fs.Lookup(name)
			if f == nil {
				hint := "; run jocasta COMMAND --help for the flags a command accepts"
				if name == "version" || name == "v" {
					hint = "; the version command is jocasta version, or jocasta --version as the first argument"
				}
				return "", o, nil, invalid("unknown flag: " + name + hint)
			}
			if !strings.Contains(a, "=") {
				if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
					i++
					if i == len(args) {
						return "", o, nil, invalid("missing value for --" + name)
					}
					flags = append(flags, args[i])
				}
			}
		} else {
			pos = append(pos, a)
		}
	}
	if e := fs.Parse(flags); e != nil {
		return "", o, nil, invalid(e.Error())
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "title" {
			o.titleSet = true
		}
		if f.Name == "kind" {
			o.kindSet = true
		}
	})
	if len(pos) == 0 {
		return "", o, nil, invalid("a command is required; use jocasta help")
	}
	return pos[0], o, pos[1:], nil
}

// Run executes one command line and returns the exit code the caller sees.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, version string) int {
	code, _ := runClassified(ctx, args, in, out, errOut, version)
	return code
}

// RunSpooled is Run with the invocation appended to the tool-feedback spool.
// It is what the jocasta binary calls and what a test of the spool calls;
// every other caller of this package goes through Run and records nothing, so
// this package's own test suite never appends to an operator's spool.
//
// The record is written between the command finishing and the caller learning
// about it, and it cannot change either: spoolInvocation prints nothing and
// returns nothing, and the exit code was decided before it ran. See spool.go.
func RunSpooled(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, version string) int {
	if descriptionRequested(args) {
		return writeDescription(out)
	}
	started := time.Now()
	code, class := runClassified(ctx, args, in, out, errOut, version)
	spoolInvocation(args, code, class, time.Since(started))
	return code
}

// runClassified is Run, and also answers the error class the spool records.
// The class cannot be derived from the exit code alone: ExitUnavailable is
// both "the server did not answer" and the default for an error that nothing
// classified, and only here is the difference still visible.
func runClassified(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, version string) (int, string) {
	if descriptionRequested(args) {
		return writeDescription(out), ""
	}
	// --version and -v name the version command rather than a flag, so they are
	// rewritten into it here: the global flagset would otherwise reject them as
	// unknown flags, and going through the command keeps --json working.
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-v") {
		args = append([]string{"version"}, args[1:]...)
	}
	// Help is answered before parse for the same reason: the flagset is global
	// and rejects an unregistered token before any verb has been chosen. A help
	// page reads no configuration and makes no request.
	if verb, ok := helpTarget(args); ok {
		code := writeHelp(out, errOut, verb)
		return code, spoolErrClass(code)
	}
	cmd, o, pos, e := parse(args)
	if e == nil && cmd == "version" {
		if o.json {
			_ = jsonOut(out, map[string]string{"version": version, "commit": buildinfo.Commit})
		} else {
			fmt.Fprintln(out, version)
		}
		return 0, ""
	}
	if e == nil {
		e = execute(ctx, cmd, o, pos, in, out, errOut)
	}
	if e == nil {
		return 0, ""
	}
	code := ExitUnavailable
	// classified records whether anything actually decided what this failure
	// was, so that the default above is not reported as a transport failure.
	classified := false
	detail := client.ErrorDetail{Code: "unavailable", Message: e.Error()}
	var le *localError
	var ae *client.Error
	if errors.As(e, &le) {
		classified = true
		if le.silent {
			return le.code, spoolErrClass(le.code)
		}
		code = le.code
		detail.Code = map[int]string{ExitInvalid: "invalid_input", ExitIO: "local_io", ExitConflict: "conflict"}[code]
	} else if errors.As(e, &ae) {
		classified = true
		detail = ae.Detail
		switch detail.Code {
		case "not_found":
			code = ExitNotFound
		case "conflict", "cursor_expired", "idempotency_conflict", "path_exists":
			code = ExitConflict
		case "auth", "unauthorized", "forbidden":
			code = ExitAuth
		case "invalid_input", "too_large", "invalid_content":
			code = ExitInvalid
		}
	}
	// A revision conflict is reconciled, never retried, so the refusal carries
	// what reconciling needs: who wrote the newer revision, and the two commands
	// that read it and show what changed, with the document and both revisions
	// already filled in. The commands are rendered here rather than sent by the
	// server, because the command line is the client's vocabulary.
	if detail.Code == "conflict" && detail.ID != "" && detail.CurrentRevision != 0 {
		detail.Resolve = []string{
			fmt.Sprintf("jocasta get --id %s --json", detail.ID),
			fmt.Sprintf("jocasta diff --id %s --from %d --to %d", detail.ID, detail.ExpectedRevision, detail.CurrentRevision),
		}
	}
	if o.json {
		_ = jsonOut(errOut, client.Error{Version: "jocasta/v1", Detail: detail})
	} else {
		fmt.Fprintln(errOut, "jocasta:", detail.Code+":", detail.Message)
		if detail.CurrentRevision != 0 {
			writer := ""
			if detail.Actor != "" {
				writer = fmt.Sprintf(", written by %s", detail.Actor)
			}
			fmt.Fprintf(errOut, "document %s: expected revision %d, current revision %d%s\n", detail.ID, detail.ExpectedRevision, detail.CurrentRevision, writer)
		}
		if len(detail.Resolve) > 0 {
			fmt.Fprintln(errOut, "reconcile with:")
			for _, command := range detail.Resolve {
				fmt.Fprintln(errOut, " ", command)
			}
		}
	}
	class := spoolErrClass(code)
	if !classified {
		class = spoolClassUnclassified
	}
	return code, class
}
func target(o options, pos []string, mutation bool) (url.Values, int64, error) {
	q := url.Values{}
	pinned := int64(0)
	if len(pos) > 1 {
		return nil, 0, invalid("expected one document reference")
	}
	if o.id != "" && len(pos) > 0 {
		return nil, 0, invalid("provide a path/reference or --id, not both")
	}
	if o.id != "" {
		q.Set("id", o.id)
	} else if len(pos) == 1 {
		ref := pos[0]
		if strings.HasPrefix(ref, "jocasta:") {
			s := strings.TrimPrefix(ref, "jocasta:")
			parts := strings.Split(s, "@")
			if len(parts) != 2 || parts[0] == "" {
				return nil, 0, invalid("exact reference must be jocasta:ID@REVISION")
			}
			n, e := strconv.ParseInt(parts[1], 10, 64)
			if e != nil || n < 1 {
				return nil, 0, invalid("invalid reference revision")
			}
			q.Set("id", parts[0])
			pinned = n
		} else {
			q.Set("path", ref)
		}
	} else {
		return nil, 0, invalid("a document path or --id is required")
	}
	if mutation {
		if pinned != 0 && o.ifRevision != 0 && pinned != o.ifRevision {
			return nil, 0, invalid("reference and --if-revision disagree")
		}
		if pinned == 0 {
			pinned = o.ifRevision
		}
	} else {
		if pinned != 0 && o.revision != 0 && pinned != o.revision {
			return nil, 0, invalid("reference and --revision disagree")
		}
		if pinned == 0 {
			pinned = o.revision
		}
		if pinned > 0 {
			q.Set("revision", strconv.FormatInt(pinned, 10))
		}
	}
	return q, pinned, nil
}
func readContent(path string, in io.Reader) ([]byte, error) {
	var r io.Reader = in
	if path == "" {
		return nil, invalid("--file is required")
	}
	if path != "-" {
		f, e := os.Open(path)
		if e != nil {
			return nil, localIO(e)
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return nil, localIO(e)
		}
		if !st.Mode().IsRegular() {
			return nil, invalid("input must be a regular file")
		}
		r = f
	}
	b, e := io.ReadAll(io.LimitReader(r, MaxDocument+1))
	if e != nil {
		return nil, localIO(e)
	}
	if len(b) > MaxDocument {
		return nil, invalid("document exceeds 16 MiB limit")
	}
	if !utf8.Valid(b) || strings.IndexByte(string(b), 0) >= 0 {
		return nil, invalid("content must be valid UTF-8 without NUL")
	}
	return b, nil
}

// writeFile writes the document bytes to path. A regular file is written
// beside its target and linked or renamed into place, so a reader never sees a
// partial document and an existing file is replaced only with --overwrite. A
// target that exists and is not a regular file (a device such as /dev/null, a
// FIFO, a socket) has nothing to link beside it and nothing to rename over,
// so it is written directly.
func writeFile(path string, b []byte, overwrite bool) error {
	if st, e := os.Stat(path); e == nil {
		if !st.Mode().IsRegular() {
			f, e := os.OpenFile(path, os.O_WRONLY, 0)
			if e != nil {
				return localIO(e)
			}
			_, e = f.Write(b)
			if ce := f.Close(); e == nil {
				e = ce
			}
			if e != nil {
				return localIO(e)
			}
			return nil
		}
		if !overwrite {
			return localIO(fmt.Errorf("output file %s exists; pass --overwrite to replace it", path))
		}
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".jocasta-output-*")
	if e != nil {
		return localIO(e)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return localIO(e)
	}
	if overwrite {
		e = os.Rename(tmp, path)
	} else {
		e = os.Link(tmp, path)
	}
	if e != nil {
		if errors.Is(e, os.ErrExist) {
			return localIO(fmt.Errorf("output file %s exists; pass --overwrite to replace it", path))
		}
		return localIO(e)
	}
	return nil
}

// exists reports whether a document is present, and nothing else. It reuses
// the get endpoint and drops the content, so the wire contract is unchanged
// and the read counts as activity exactly as a get would. A reference without
// a revision, jocasta:ID, is accepted here because presence has no revision.
func exists(ctx context.Context, c *client.Client, o options, pos []string, out io.Writer) error {
	if len(pos) == 1 && strings.HasPrefix(pos[0], "jocasta:") && !strings.Contains(pos[0], "@") {
		if o.id != "" {
			return invalid("provide a path/reference or --id, not both")
		}
		o.id = strings.TrimPrefix(pos[0], "jocasta:")
		pos = nil
	}
	q, _, e := target(o, pos, false)
	if e != nil {
		return e
	}
	var r Receipt
	e = c.Call(ctx, http.MethodGet, "/v1/get", q, nil, "", &r)
	var ae *client.Error
	if errors.As(e, &ae) && ae.Detail.Code == "not_found" {
		if o.json {
			if e := jsonOut(out, map[string]any{"version": api.Version, "exists": false}); e != nil {
				return e
			}
		}
		return &localError{code: ExitNotFound, err: e, silent: true}
	}
	if e != nil {
		return e
	}
	if o.json {
		return jsonOut(out, map[string]any{"version": api.Version, "exists": true, "document": map[string]any{
			"id": r.Document.ID, "path": r.Document.Path, "revision": r.Document.Revision, "archived": r.Document.Archived,
		}})
	}
	_, e = fmt.Fprintf(out, "jocasta:%s@%d  %s\n", r.Document.ID, r.Document.Revision, r.Document.Path)
	return e
}
func execute(ctx context.Context, cmd string, o options, pos []string, in io.Reader, out, diagnostics io.Writer) error {
	switch cmd {
	case "put", "get", "exists", "ls", "search", "history", "diff", "mv", "archive", "unarchive", "restore", "import":
	default:
		return invalid("unknown command: " + cmd)
	}
	if o.revision < 0 || o.ifRevision < 0 || o.from < 0 || o.to < 0 {
		return invalid("revisions must be positive")
	}
	if cmd == "import" && o.dryRun {
		return inventory(o, pos, out)
	}
	conf, e := client.LoadConfig(o.config)
	if e != nil {
		return localIO(e)
	}
	c, e := client.New(conf)
	if e != nil {
		return invalid(e.Error())
	}
	if cmd == "import" {
		return importManifest(ctx, c, o, pos, out)
	}
	if cmd == "exists" {
		return exists(ctx, c, o, pos, out)
	}
	if cmd == "get" || cmd == "diff" {
		q, _, e := target(o, pos, false)
		if e != nil {
			return e
		}
		if cmd == "diff" {
			return diff(ctx, c, q, o, out, diagnostics)
		}
		if o.lines != "" {
			if !o.json || o.output != "" {
				return invalid("--lines requires --json and disallows --output")
			}
			parts := strings.Split(o.lines, ":")
			if len(parts) != 2 {
				return invalid("lines must be START:END")
			}
			a, e1 := strconv.Atoi(parts[0])
			b, e2 := strconv.Atoi(parts[1])
			if e1 != nil || e2 != nil || a < 1 || b < a {
				return invalid("invalid line range")
			}
			q.Set("lines", o.lines)
		}
		var r Receipt
		if e = c.Call(ctx, http.MethodGet, "/v1/get", q, nil, "", &r); e != nil {
			return e
		}
		if !o.json && r.Document.Archived {
			fmt.Fprintf(diagnostics, "jocasta: archived document %s at revision %d\n", r.Document.ID, r.Document.Revision)
		}
		if o.output != "" {
			if e = writeFile(o.output, []byte(r.Document.Content), o.overwrite); e != nil {
				return e
			}
			if o.json {
				var m map[string]any
				b, _ := json.Marshal(r)
				_ = json.Unmarshal(b, &m)
				delete(m["document"].(map[string]any), "content")
				return jsonOut(out, m)
			}
			return nil
		}
		if o.json {
			return jsonOut(out, r)
		}
		_, e = io.WriteString(out, r.Document.Content)
		if e != nil {
			return localIO(e)
		}
		return nil
	}
	if cmd == "ls" || cmd == "search" || cmd == "history" {
		if o.limit < 1 || o.limit > 200 {
			return invalid("--limit must be between 1 and 200")
		}
		q := url.Values{"limit": {strconv.Itoa(o.limit)}, "cursor": {o.cursor}, "prefix": {o.prefix}, "project": {o.project}, "kind": {o.kind}, "include_archived": {strconv.FormatBool(o.includeArchived)}}
		endpoint := "/v1/list"
		if cmd == "ls" {
			if len(pos) > 1 {
				return invalid("ls accepts at most one prefix")
			}
			if len(pos) == 1 {
				q.Set("prefix", pos[0])
			}
		}
		if cmd == "search" {
			if len(pos) != 1 {
				return invalid("search requires one query")
			}
			if o.mode != "default" && o.mode != "literal" {
				return invalid("search mode must be default or literal")
			}
			endpoint = "/v1/search"
			q.Set("q", pos[0])
			q.Set("mode", o.mode)
		}
		if cmd == "history" {
			t, _, e := target(o, pos, false)
			if e != nil {
				return e
			}
			for k, v := range t {
				q[k] = v
			}
			endpoint = "/v1/history"
		}
		var p Page
		if e = c.Call(ctx, http.MethodGet, endpoint, q, nil, "", &p); e != nil {
			return e
		}
		if o.json {
			return jsonOut(out, p)
		}
		for _, d := range p.Documents {
			state := ""
			if d.Archived {
				state = "  [archived]"
			}
			if _, e := fmt.Fprintf(out, "%s  r%d  %s%s\n", d.ID, d.Revision, d.Path, state); e != nil {
				return localIO(e)
			}
			if cmd == "search" {
				if _, e := fmt.Fprintf(out, "  %q\n  %q\n", d.Title, d.Content); e != nil {
					return localIO(e)
				}
			}
		}
		if p.NextCursor != "" {
			fmt.Fprintf(out, "next_cursor: %s\n", p.NextCursor)
		}
		if !p.Complete || p.HasMore {
			fmt.Fprintf(out, "partial results: has_more=%t complete=%t scanned_count=%d\n", p.HasMore, p.Complete, p.ScannedCount)
		}
		if p.RankingReason != "" {
			fmt.Fprintf(diagnostics, "jocasta: the full-text index could not rank this query (%s); results are ordered by name match alone\n", p.RankingReason)
		}
		return nil
	}
	refs := pos
	destination := ""
	if cmd == "mv" {
		if o.id != "" {
			if len(pos) != 1 {
				return invalid("mv --id requires destination")
			}
			destination = pos[0]
			refs = nil
		} else {
			if len(pos) != 2 {
				return invalid("mv requires source and destination")
			}
			destination = pos[1]
			refs = pos[:1]
		}
	}
	q, expected, e := target(o, refs, true)
	if e != nil {
		return e
	}
	if cmd == "restore" && len(refs) > 0 && strings.HasPrefix(refs[0], "jocasta:") {
		return invalid("restore requires unpinned target and separate --revision")
	}
	operation := cmd
	if cmd == "mv" {
		operation = "move"
	}
	if cmd == "put" && o.create {
		operation = "create"
		if expected != 0 || q.Get("id") != "" {
			return invalid("--create requires path and cannot use an expected revision")
		}
	} else if expected < 1 {
		return invalid("mutation requires --if-revision or pinned exact reference")
	}
	body := map[string]any{"operation": operation, "path": q.Get("path"), "id": q.Get("id"), "if_revision": expected, "message": o.message}
	if cmd == "put" {
		b, e := readContent(o.file, in)
		if e != nil {
			return e
		}
		body["content"] = string(b)
		if o.titleSet {
			body["title"] = o.title
		}
		if o.kindSet {
			body["kind"] = o.kind
		}
	}
	if cmd == "mv" {
		body["destination"] = destination
	}
	if cmd == "restore" {
		if o.revision < 1 {
			return invalid("restore requires source --revision")
		}
		body["source_revision"] = o.revision
	}
	id := o.requestID
	if id == "" {
		id = client.RequestID()
	}
	var r Receipt
	if e = c.Call(ctx, http.MethodPost, "/v1/mutate", nil, body, id, &r); e != nil {
		return e
	}
	if o.verbose {
		// The write answered with the new revision alone. --verbose reads that
		// exact revision back, which is immutable, so the content reported is
		// what the server stored rather than an echo of what was sent.
		var full Receipt
		back := url.Values{"id": {r.Document.ID}, "revision": {strconv.FormatInt(r.Document.Revision, 10)}}
		if e = c.Call(ctx, http.MethodGet, "/v1/get", back, nil, "", &full); e != nil {
			return e
		}
		r.Document.Content = full.Document.Content
	}
	if o.json {
		return jsonOut(out, r)
	}
	_, e = fmt.Fprintf(out, "jocasta:%s@%d  %s  %s\n", r.Document.ID, r.Document.Revision, r.Document.Path, r.Document.ContentHash)
	return e
}
func diff(ctx context.Context, c *client.Client, q url.Values, o options, out, diagnostics io.Writer) error {
	if o.from < 1 || o.to < 1 {
		return invalid("diff requires positive --from and --to")
	}
	if q.Get("revision") != "" {
		return invalid("diff requires an unpinned reference")
	}
	var a, b Receipt
	q.Set("revision", strconv.FormatInt(o.from, 10))
	if e := c.Call(ctx, http.MethodGet, "/v1/get", q, nil, "", &a); e != nil {
		return e
	}
	q.Set("revision", strconv.FormatInt(o.to, 10))
	if e := c.Call(ctx, http.MethodGet, "/v1/get", q, nil, "", &b); e != nil {
		return e
	}
	// The guard bounds the work before any of it is done: two revisions this
	// large are a download, not a reconciliation.
	if len(a.Document.Content)+len(b.Document.Content) > 1<<20 {
		return invalid("diff too_large: combined revisions exceed 1 MiB")
	}
	text, hunks, bounded := unified(a.Document.Path, o.from, b.Document.Path, o.to, splitLines(a.Document.Content), splitLines(b.Document.Content))
	if o.json {
		return jsonOut(out, map[string]any{"version": "jocasta/v1", "id": a.Document.ID, "from": o.from, "to": o.to, "diff": text, "hunks": hunks, "bounded": bounded, "truncated": false, "metadata_changed": a.Document.Path != b.Document.Path || a.Document.Title != b.Document.Title || a.Document.Kind != b.Document.Kind || a.Document.Archived != b.Document.Archived})
	}
	if bounded {
		fmt.Fprintln(diagnostics, "jocasta: the revisions differ too widely for a minimal diff; the changed region is shown as one replacement")
	}
	_, e := io.WriteString(out, text)
	return e
}
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	a := strings.SplitAfter(s, "\n")
	if a[len(a)-1] == "" {
		a = a[:len(a)-1]
	}
	return a
}
