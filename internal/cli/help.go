package cli

// This file holds the command interface's reference documentation: the
// top-level summary screen and one page per verb. It deliberately imports
// neither net/http nor the client package, so no help path can reach the
// network, load a configuration file or need a token.

import (
	"fmt"
	"io"
	"strings"
)

// exitNote is one exit code a verb can return, with what it means for that
// verb. The global list on the summary screen says what a code means in
// general; a page says which codes this verb actually produces.
type exitNote struct {
	code    int
	meaning string
}

// helpTopic is one verb's reference page. The flags are named, not described:
// their descriptions and defaults are rendered from the flagset the parser
// builds, so a page cannot disagree with the flags the CLI really accepts.
type helpTopic struct {
	verb       string
	purpose    string
	usage      []string
	flags      []string
	guarantees []string
	// guard renders the shared guard contract, which every verb that writes a
	// document through an expected revision obeys.
	guard bool
	exits []exitNote
}

// guardContract is the contract shared by every guarded mutation. It is the
// part of the interface an agent must understand before it writes anything:
// what the expected revision asserts, what a pinned reference does, what a
// conflict means, and what is true of the document when a write is refused.
var guardContract = []string{
	"--if-revision N asserts that N is the current revision of the target document. The",
	"server compares it with the current revision inside the same transaction that would",
	"perform the write, so no other writer can slip between the check and the write.",
	"",
	"A pinned exact reference, jocasta:ID@REVISION, makes the same assertion on a mutation:",
	"the pinned revision is the expected current revision, not a historical revision to",
	"read. Use one or the other. A pin and an --if-revision that disagree are refused",
	"locally, before any request is sent. Without either, the mutation is refused locally",
	"with exit 6 and nothing is sent.",
	"",
	"A guard that does not hold is a conflict: exit 3, and the message names the guard --",
	"the document ID, the revision you expected and the current revision. The document is",
	"unchanged: no new revision was created, no content was written, no history entry was",
	"added, and the current revision is exactly the one the message reports.",
	"",
	"The CLI never retries a conflict and never overwrites newer content to make a retry",
	"succeed. Recover by rereading the current revision (jocasta get jocasta:ID@CURRENT),",
	"reconciling your change against it, and writing again with --if-revision CURRENT.",
	"",
	"Every mutation carries an idempotency key. Pass --request-id KEY to choose it: the same",
	"key with the same request returns the original receipt instead of writing twice, and",
	"the same key with a different request is refused with exit 3.",
	"",
	"Exit 5 means the service could not be reached or failed, and the write may or may not",
	"have been committed. Reread the document before retrying, or retry with the same",
	"--request-id, which commits at most once.",
}

// configIO is the exit 7 that every verb but version shares. execute loads the
// configuration file before it looks at a verb's own arguments, so a host that
// has never been configured gets this exit from all of them, and it is the
// first refusal such a host sees. Only version and import --dry-run return
// without reading it.
const configIO = "the configuration file named by --config could not be read or is not valid JSON"

// authRefused is the exit 4 that every verb making a request shares. A token
// file that cannot be read is a different failure: nothing is sent, and the
// refusal is local, with exit 6.
const authRefused = "the server refused the credentials in the token file (an unreadable token file is exit 6)"

// helpTopics is the reference, one entry per verb, in the order the summary
// screen lists them. Every verb the dispatcher accepts has an entry; a test
// derives the verb list from the dispatch switch and fails when one is missing.
var helpTopics = []helpTopic{
	{
		verb:    "put",
		purpose: "write a new revision of an existing document, or create a new one",
		usage: []string{
			"jocasta put PATH --file FILE --if-revision N",
			"jocasta put jocasta:ID@REVISION --file FILE",
			"jocasta put --id ID --file FILE --if-revision N",
			"jocasta put PATH --file FILE --create",
			"jocasta put PATH --file - --if-revision N      (content read from stdin)",
		},
		flags: []string{"file", "if-revision", "create", "id", "message", "title", "kind", "request-id", "verbose", "json", "config"},
		guarantees: []string{
			"The stored content is the exact bytes of --file: valid UTF-8 without NUL, at most",
			"16 MiB, never reformatted and never given or denied a trailing newline.",
			"",
			"--title and --kind are sent only when you pass them. Metadata you leave out is",
			"preserved, never blanked.",
			"",
			"Content identical to the current revision is accepted and creates no new revision;",
			"the receipt then reports the unchanged revision.",
			"",
			"--create writes a new document and requires a free path and no expected revision. A",
			"path already in use, including as an alias left behind by a move, is refused with",
			"exit 3 and creates nothing.",
			"",
			"On success the receipt names the document ID, the new revision, the path and the",
			"content hash; --json prints the versioned receipt on stdout.",
			"",
			"The receipt does not carry the content back. Its answer is the new revision, and the",
			"content hash and line count are there to verify the write. Read the bytes with get,",
			"or pass --verbose, which reads the revision just written back and reports the whole",
			"document in the receipt.",
		},
		guard: true,
		exits: []exitNote{
			{ExitOK, "the write was committed; the receipt reports the resulting revision"},
			{ExitNotFound, "the path or --id names no document (an update; --create does not need one)"},
			{ExitConflict, "the guard did not hold, --create found the path taken, or --request-id was reused with different input; nothing was written"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed; the write may or may not have been committed"},
			{ExitInvalid, "refused before any write: no expected revision, --create with one, a bad reference, or content that is not UTF-8 or exceeds 16 MiB"},
			{ExitIO, configIO + ", or the local --file could not be read"},
		},
	},
	{
		verb:    "get",
		purpose: "write a document's exact content to stdout or to a file",
		usage: []string{
			"jocasta get PATH",
			"jocasta get jocasta:ID@REVISION [--json]",
			"jocasta get --id ID [--revision N] [--json]",
			"jocasta get PATH --output FILE [--overwrite] [--json]",
			"jocasta get PATH --lines START:END --json",
		},
		flags: []string{"id", "revision", "output", "overwrite", "lines", "json", "config"},
		guarantees: []string{
			"Without --json the exact bytes go to stdout and nothing else does; notices such as",
			"the archived warning go to stderr, so a redirected stdout is the document.",
			"",
			"--output FILE writes those same bytes. An existing regular file is refused with exit",
			"7 unless --overwrite replaces it atomically; a device such as /dev/null is written",
			"directly. With --json the receipt is printed without duplicating the content.",
			"",
			"--revision N and jocasta:ID@N both read that immutable historical revision, and the",
			"two are refused when they disagree. Revisions are never rewritten.",
			"",
			"--lines START:END requires --json and forbids --output. It is an explicit partial",
			"view: the receipt marks it partial and carries the full document hash. Never write a",
			"partial view back as a replacement document.",
			"",
			"A successful read records activity, which resets the 30-day inactivity window, but",
			"does not unarchive an archived document.",
		},
		exits: []exitNote{
			{ExitOK, "the content was delivered"},
			{ExitNotFound, "no such document, or no such revision of it"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "a bad reference, a contradictory revision, or an invalid --lines range; no request was made"},
			{ExitIO, configIO + ", or --output exists without --overwrite, or the file could not be written"},
		},
	},
	{
		verb:    "exists",
		purpose: "report whether a document is present, without fetching its content",
		usage: []string{
			"jocasta exists PATH [--json]",
			"jocasta exists jocasta:ID [--json]",
			"jocasta exists jocasta:ID@REVISION [--json]",
			"jocasta exists --id ID [--json]",
		},
		flags: []string{"id", "json", "config"},
		guarantees: []string{
			"Presence only: exit 0 when the document is present, 2 when it is absent. Absence is",
			"an answer, not a failure, and nothing is printed on stderr for it.",
			"",
			"--json prints {\"version\":\"jocasta/v1\",\"exists\":true|false,...} on stdout, including",
			"for an absent document, where the exit code is still 2.",
			"",
			"A reference without a revision, jocasta:ID, is accepted here, because presence has no",
			"revision.",
			"",
			"The check reads the document server-side and so records activity exactly as get does.",
			"Prefer it to get --output /dev/null as the condition of a wait check.",
		},
		exits: []exitNote{
			{ExitOK, "the document is present; the line names its exact reference and path"},
			{ExitNotFound, "the document is absent"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "a bad reference, or both a reference and --id"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "ls",
		purpose: "list documents, optionally under a path prefix",
		usage: []string{
			"jocasta ls [PREFIX] [--limit N] [--cursor CURSOR] [--json]",
			"jocasta ls --prefix PREFIX --project PROJECT --kind KIND [--include-archived]",
		},
		flags: []string{"prefix", "project", "kind", "include-archived", "limit", "cursor", "json", "config"},
		guarantees: []string{
			"A positional PREFIX and --prefix mean the same thing; the positional wins.",
			"",
			"--limit must be between 1 and 200. A page reports has_more, next_cursor and complete:",
			"continue with --cursor, and treat an incomplete page as an incomplete answer rather",
			"than as proof that nothing matches.",
			"",
			"A cursor binds the filters and ordering of the listing it came from, and a write",
			"expires it. An expired cursor is refused with exit 3; start the listing again.",
			"",
			"Archived documents are omitted unless --include-archived is given.",
		},
		exits: []exitNote{
			{ExitOK, "the page was delivered, complete or not"},
			{ExitConflict, "the cursor expired because the corpus changed"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "--limit out of range, or more than one prefix"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "search",
		purpose: "find documents by a lexical query over titles, paths and content",
		usage: []string{
			"jocasta search QUERY [--json]",
			"jocasta search QUERY --mode literal [--project PROJECT] [--kind KIND]",
			"jocasta search QUERY --limit N --cursor CURSOR [--include-archived]",
		},
		flags: []string{"mode", "project", "kind", "prefix", "include-archived", "limit", "cursor", "json", "config"},
		guarantees: []string{
			"Exactly one QUERY is required. --mode default treats it as text and never as raw FTS",
			"or SQL syntax; --mode literal matches punctuation-rich text exactly.",
			"",
			"Search is lexical. It finds shared words, not shared concepts, so a bounded or",
			"zero-match page is not proof that a document does not exist.",
			"",
			"Results are ranked: a document the query names by its own path or title outranks one",
			"that only mentions the same words, and the full-text index scores the rest. Equal",
			"scores are ordered by recency and then by path. Scores are relative to the corpus, so",
			"adding or removing documents can change the order of the results between them.",
			"",
			"--json reports ranking: bm25 when the index scored the page, literal for --mode",
			"literal, and substring with a ranking_reason when the index could not answer and",
			"name matching alone ordered the page.",
			"",
			"Titles and paths of archived documents are matched by default; their content requires",
			"--include-archived.",
			"",
			"Pagination behaves as it does for ls: has_more, next_cursor and complete, --cursor to",
			"continue, and exit 3 when a write has expired the cursor.",
		},
		exits: []exitNote{
			{ExitOK, "the page was delivered, complete or not"},
			{ExitConflict, "the cursor expired because the corpus changed"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "no query, more than one query, --limit out of range, or an unknown --mode"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "history",
		purpose: "list the immutable revisions of one document",
		usage: []string{
			"jocasta history PATH [--limit N] [--cursor CURSOR] [--json]",
			"jocasta history --id ID [--json]",
		},
		flags: []string{"id", "limit", "cursor", "json", "config"},
		guarantees: []string{
			"Exactly one document: a path or --id, not both.",
			"",
			"Pagination anchors to the highest revision that existed when the first page was",
			"taken, so revisions written while you page do not shift the pages under you.",
			"",
			"A history cursor therefore never expires. A revision written between two pages does",
			"not invalidate the cursor and does not appear in this enumeration; paging continues",
			"from the anchor and reaches the end of the history as it stood at the first page.",
			"Start a fresh history to see revisions written since. A cursor does belong to the",
			"document and the --limit it came from: reused with another document or another",
			"--limit, or malformed, it is refused with exit 6.",
			"",
			"History is never rewritten and never automatically deleted; archiving and restoring",
			"both append revisions rather than removing any.",
		},
		exits: []exitNote{
			{ExitOK, "the page was delivered"},
			{ExitNotFound, "no such document"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "a bad reference, both a reference and --id, --limit out of range, or a malformed or mismatched --cursor"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "diff",
		purpose: "show what changed between two revisions of one document",
		usage: []string{
			"jocasta diff PATH --from N --to M [--json]",
			"jocasta diff --id ID --from N --to M [--json]",
		},
		flags: []string{"id", "from", "to", "json", "config"},
		guarantees: []string{
			"--from and --to are both required and both positive. The document reference must be",
			"unpinned: a jocasta:ID@REVISION target is refused, because the revisions to compare",
			"are the two flags.",
			"",
			"The output is a unified diff: only the lines that changed, each in a hunk with up to",
			"three unchanged lines of context on either side. Its size follows the change and not",
			"the document, so one changed line in a large document is one small hunk. Two",
			"revisions whose combined size exceeds 1 MiB are refused rather than compared.",
			"",
			"Revisions that differ too widely for a minimal diff are reported as one replacement",
			"of the region that differs, with a notice on stderr and bounded true in --json.",
			"",
			"Identical revisions produce empty output, no hunks and exit 0. With --json, hunks",
			"counts them and metadata_changed reports whether path, title, kind or archive state",
			"differ.",
			"",
			"diff reads only; it never writes a revision.",
		},
		exits: []exitNote{
			{ExitOK, "the comparison was delivered, empty when the revisions are identical"},
			{ExitNotFound, "no such document, or no such revision of it"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "a missing or non-positive --from or --to, a pinned reference, or a comparison over 1 MiB"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "mv",
		purpose: "move a document to a new path, keeping its identity and history",
		usage: []string{
			"jocasta mv PATH DESTINATION --if-revision N",
			"jocasta mv jocasta:ID@REVISION DESTINATION",
			"jocasta mv --id ID DESTINATION --if-revision N",
		},
		flags: []string{"id", "if-revision", "message", "request-id", "verbose", "json", "config"},
		guarantees: []string{
			"The document keeps its ID, its whole revision history and its archive state. The old",
			"path stays a permanent alias, so references and reads by the old path keep resolving.",
			"",
			"A move appends a new revision; content is unchanged.",
			"",
			"The destination must be free. A path or alias another document already owns is",
			"refused with exit 3, and the document stays where it was.",
		},
		guard: true,
		exits: []exitNote{
			{ExitOK, "the move was committed; the receipt reports the new path and revision"},
			{ExitNotFound, "no such source document"},
			{ExitConflict, "the guard did not hold, or the destination path or alias is taken; nothing was moved"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed; the move may or may not have been committed"},
			{ExitInvalid, "a missing destination, a missing expected revision, or a bad reference"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "archive",
		purpose: "archive a document, which keeps it searchable and retrievable",
		usage: []string{
			"jocasta archive PATH --if-revision N",
			"jocasta archive jocasta:ID@REVISION",
			"jocasta archive --id ID --if-revision N",
		},
		flags: []string{"id", "if-revision", "message", "request-id", "verbose", "json", "config"},
		guarantees: []string{
			"Archiving deletes nothing. The document keeps its ID, path, content and history, and",
			"stays retrievable by get; search still matches its title and path.",
			"",
			"Archiving appends a new revision.",
			"",
			"Documents also archive automatically after 30 days without a read or an update; a",
			"read resets that window without unarchiving.",
		},
		guard: true,
		exits: []exitNote{
			{ExitOK, "the document is archived"},
			{ExitNotFound, "no such document"},
			{ExitConflict, "the guard did not hold; the document is unchanged"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed; the change may or may not have been committed"},
			{ExitInvalid, "a missing expected revision or a bad reference"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "unarchive",
		purpose: "return an archived document to active state",
		usage: []string{
			"jocasta unarchive PATH --if-revision N",
			"jocasta unarchive jocasta:ID@REVISION",
			"jocasta unarchive --id ID --if-revision N",
		},
		flags: []string{"id", "if-revision", "message", "request-id", "verbose", "json", "config"},
		guarantees: []string{
			"Unarchiving appends a new revision and starts a fresh 30-day inactivity window.",
			"",
			"Content, path, ID and history are untouched; only the archive state changes.",
			"",
			"Unarchiving a document that is already active changes nothing and creates no",
			"revision.",
		},
		guard: true,
		exits: []exitNote{
			{ExitOK, "the document is active"},
			{ExitNotFound, "no such document"},
			{ExitConflict, "the guard did not hold; the document is unchanged"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed; the change may or may not have been committed"},
			{ExitInvalid, "a missing expected revision or a bad reference"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "restore",
		purpose: "copy an earlier revision's content into a new current revision",
		usage: []string{
			"jocasta restore PATH --revision SOURCE --if-revision CURRENT",
			"jocasta restore --id ID --revision SOURCE --if-revision CURRENT",
		},
		flags: []string{"id", "revision", "if-revision", "message", "request-id", "verbose", "json", "config"},
		guarantees: []string{
			"Restore never rewinds and never deletes: it appends a new revision whose content,",
			"title and kind are copied from --revision SOURCE. Everything between SOURCE and the",
			"current revision stays in history.",
			"",
			"The current path and archive state are kept; a restore does not move or unarchive.",
			"",
			"The target must be unpinned. Name the document as PATH or --id, the revision to copy",
			"as --revision SOURCE, and the guard as --if-revision CURRENT; a jocasta:ID@REVISION",
			"target is refused with exit 6, because the two revisions mean different things.",
		},
		guard: true,
		exits: []exitNote{
			{ExitOK, "the restored content is the new current revision"},
			{ExitNotFound, "no such document, or no such source revision"},
			{ExitConflict, "the guard did not hold; the document is unchanged"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed; the restore may or may not have been committed"},
			{ExitInvalid, "a pinned target, a missing or non-positive --revision, or a missing expected revision"},
			{ExitIO, configIO},
		},
	},
	{
		verb:    "import",
		purpose: "import a directory of files as new documents, in two reviewed steps",
		usage: []string{
			"jocasta import ROOT --dry-run --migration-id ID --manifest FILE [--prefix legacy]",
			"jocasta import ROOT --manifest FILE --migration-id ID",
		},
		flags: []string{"dry-run", "manifest", "migration-id", "prefix", "overwrite", "config"},
		guarantees: []string{
			"--dry-run only inventories. It makes no request and writes no document: it prints the",
			"manifest and, with --manifest FILE, writes it for review. The manifest must live",
			"outside ROOT, and an existing manifest file is replaced only with --overwrite.",
			"",
			"The importing step re-scans the source and refuses with exit 3 if anything changed",
			"since the manifest was taken, both before it writes anything and after it finishes.",
			"",
			"Every entry is created, never updated. Each written document is read back and",
			"verified byte for byte against the manifest hash before the import continues.",
			"",
			"Idempotency keys are derived from the migration ID, the source path and the content",
			"hash, so importing the same manifest twice does not write the same file twice.",
			"",
			"Source files are never modified. Symlinks, irregular files, unreadable entries,",
			"invalid destination paths and files over 16 MiB are skipped and listed as skipped.",
			"",
			"Output is always JSON, with or without --json.",
		},
		exits: []exitNote{
			{ExitOK, "the inventory or the import finished and was verified"},
			{ExitNotFound, "a document written during the import could not be read back"},
			{ExitConflict, "the source changed since the manifest, a destination was taken, or read-back verification failed; the import stopped there"},
			{ExitAuth, authRefused},
			{ExitUnavailable, "the service was unreachable or failed"},
			{ExitInvalid, "a missing --manifest or --migration-id, a manifest that does not match ROOT, or an invalid prefix or destination"},
			{ExitIO, configIO + ", or the source root or the manifest could not be read or written; --dry-run reads no configuration"},
		},
	},
	{
		verb:    "version",
		purpose: "print the CLI version",
		usage: []string{
			"jocasta version [--json]",
			"jocasta --version [--json]",
			"jocasta -v [--json]",
		},
		flags: []string{"json"},
		guarantees: []string{
			"Reads no configuration, needs no token and makes no request.",
			"",
			"--json prints {\"version\":...,\"commit\":...}; without it, the version alone.",
			"",
			"--version and -v are accepted as the first argument and mean this command.",
		},
		exits: []exitNote{
			{ExitOK, "the version was printed"},
			{ExitInvalid, "an unknown flag was given"},
		},
	},
}

func topicFor(verb string) *helpTopic {
	for i := range helpTopics {
		if helpTopics[i].verb == verb {
			return &helpTopics[i]
		}
	}
	return nil
}

func verbList() string {
	names := make([]string, 0, len(helpTopics))
	for _, t := range helpTopics {
		names = append(names, t.verb)
	}
	return strings.Join(names, " ")
}

// helpTarget reports whether these arguments ask for help and, if so, for
// which verb; an empty verb means the summary screen. It walks the arguments
// the way parse does, skipping the value of a non-boolean flag, so a -h that
// is the value of --message is a value and not a request for help.
func helpTarget(args []string) (string, bool) {
	if len(args) == 0 {
		return "", true
	}
	var o options
	fs := newFlagSet(&o)
	var pos []string
	asked := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		switch {
		case a == "--help" || a == "-help" || a == "-h":
			asked = true
		case strings.HasPrefix(a, "-") && a != "-":
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			if f := fs.Lookup(name); f != nil && !strings.Contains(a, "=") {
				if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
					i++
				}
			}
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) > 0 && pos[0] == "help" {
		if len(pos) > 1 {
			return pos[1], true
		}
		return "", true
	}
	if asked {
		if len(pos) > 0 {
			return pos[0], true
		}
		return "", true
	}
	return "", false
}

// writeHelp prints the summary screen or one verb's page on out and returns the
// exit code. It performs no request and reads no configuration, so it works on
// a host with no Jocasta credentials at all.
func writeHelp(out, errOut io.Writer, verb string) int {
	if verb == "" {
		writeOverview(out)
		return ExitOK
	}
	t := topicFor(verb)
	if t == nil {
		fmt.Fprintf(errOut, "jocasta: invalid_input: no help for %q; commands are: %s\n", verb, verbList())
		return ExitInvalid
	}
	writePage(out, t)
	return ExitOK
}

func writePage(w io.Writer, t *helpTopic) {
	var o options
	fs := newFlagSet(&o)
	fmt.Fprintf(w, "jocasta %s — %s\n", t.verb, t.purpose)
	fmt.Fprintln(w, "\nUsage")
	for _, u := range t.usage {
		fmt.Fprintf(w, "  %s\n", u)
	}
	fmt.Fprintln(w, "\nFlags")
	for _, name := range t.flags {
		f := fs.Lookup(name)
		if f == nil {
			fmt.Fprintf(w, "  --%s\n", name)
			continue
		}
		d := f.DefValue
		if d == "" {
			d = `""`
		}
		fmt.Fprintf(w, "  --%-17s %s (default %s)\n", name, f.Usage, d)
	}
	writeSection(w, "Guarantees", t.guarantees)
	if t.guard {
		writeSection(w, "Guard contract", guardContract)
	}
	fmt.Fprintln(w, "\nExit codes")
	for _, x := range t.exits {
		fmt.Fprintf(w, "  %d  %s\n", x.code, x.meaning)
	}
	fmt.Fprintf(w, "\nThis page makes no request and needs no credentials. Run jocasta help for the\ncommand list, and jocasta COMMAND --help for another command.\n")
}

func writeSection(w io.Writer, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, l := range lines {
		if l == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "  %s\n", l)
	}
}

// writeOverview prints the summary screen. It stays a summary: the reference
// for a command is that command's own page.
func writeOverview(w io.Writer) {
	fmt.Fprintln(w, "Jocasta — versioned shared documents\nCommands: put get exists ls search history diff mv archive unarchive restore import version\nRun jocasta COMMAND --help, jocasta COMMAND -h or jocasta help COMMAND for that command's flags, guarantees and exit codes.\nUse --config FILE to select configuration; --json for machine output.\nput PATH --file FILE (--create | --if-revision N)\nget PATH|jocasta:ID@REV [--json] [--output FILE [--overwrite]]\n  --output writes exact content to FILE. An existing regular file is refused unless --overwrite replaces it atomically; a device such as /dev/null is written directly.\nget --id ID [--revision N] [--lines START:END --json]\nexists PATH|jocasta:ID[@REV] [--json]\n  Presence only, no content: exit 0 when present, 2 when absent. --json prints {\"exists\":true|false,\"document\":{id,path,revision,archived}}.\nls [PREFIX] [--limit N --cursor CURSOR]\nsearch QUERY [--mode default|literal] [--project PROJECT]\nhistory PATH|--id ID\ndiff PATH|--id ID --from N --to N\nmv PATH DESTINATION --if-revision N\narchive|unarchive PATH|--id ID --if-revision N\nrestore PATH|--id ID --revision SOURCE --if-revision CURRENT\nimport ROOT --dry-run --migration-id ID --manifest FILE [--prefix legacy]\nimport ROOT --manifest FILE --migration-id ID\nDocuments inactive for 30 days archive automatically; search still finds archived titles/paths. Reads reset inactivity without unarchiving.\nExit codes: 0 success; 2 not found; 3 conflict; 4 auth; 5 unavailable; 6 invalid input; 7 local I/O.")
}
