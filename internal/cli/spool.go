package cli

// Per-invocation instrumentation spool: one JSON line per run of the jocasta
// command, appended to the same tool-feedback spool the fleet's MCP servers
// already write. Until now every claim about how this CLI behaves for an agent
// rested on a scheduled probe; this records what actually happened, one line
// per call, on every host and from every harness.
//
// Three properties decide whether the spool is worth shipping, and each one is
// enforced here rather than left to the caller.
//
// It carries no argument values. The record holds the verb, the flag keys and
// an outcome class, and nothing else that came from the command line. A value
// can be a document, a search query, a path or a token, and these records are
// shipped to another host, so the safe set is the set of names: the verb is
// recorded only when it names a help topic this program publishes, and a flag
// key only when the parser's own flagset declares it. An error is recorded as
// a class drawn from a closed vocabulary rather than as its message, because a
// message quotes what the caller typed and what the server answered.
//
// It never changes what the caller sees. Every failure in here - no directory,
// a full disk, a read-only home - is dropped in silence. Losing an event is
// better than turning a working command into a failing one, and a warning on
// standard error would corrupt the output of every verb an agent parses.
//
// Its cost is bounded. One O_APPEND write of a short line, no fsync, no
// network, no retry, and a record that exceeds spoolRecordLimit is dropped.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/jocasta/internal/buildinfo"
)

// spoolSource names this tool's directory in the spool. Every tool that writes
// the spool has its own, so one report can rank them side by side.
const spoolSource = "jocasta"

const (
	// spoolMaxArgs bounds the flag keys recorded for one call.
	spoolMaxArgs = 24
	// spoolMaxKeyLen bounds one flag key. A longer word is not a flag this
	// program declares, so it is not recorded as one.
	spoolMaxKeyLen = 40
	// spoolMaxIdentLen bounds a session or client identifier that crossed a
	// process boundary, because it is written into the line.
	spoolMaxIdentLen = 128
	// spoolRecordLimit bounds the encoded record. A line over it is dropped:
	// the spool is a measurement, not a log.
	spoolRecordLimit = 4096
	// spoolUndeclaredFlag stands for a word that was shaped like a flag and is
	// not one this program declares. Recording the word itself would be
	// recording whatever the caller put after a dash.
	spoolUndeclaredFlag = "?"
)

// spoolRecord is one line of the spool. The field names and their order are
// the shape the tool-feedback reader already consumes from the MCP producers;
// exit is this producer's addition, because a CLI has an exit code and an MCP
// server does not.
type spoolRecord struct {
	TS      string   `json:"ts"`
	Host    string   `json:"host"`
	Session string   `json:"session"`
	Proc    string   `json:"proc"`
	Client  string   `json:"client,omitempty"`
	Ver     string   `json:"ver,omitempty"`
	Seq     int      `json:"seq"`
	Tool    string   `json:"tool"`
	Args    []string `json:"args,omitempty"`
	Outcome string   `json:"outcome"`
	OK      bool     `json:"ok"`
	Err     string   `json:"err,omitempty"`
	Exit    int      `json:"exit"`
	Ms      int64    `json:"ms"`
}

// The five outcome classes an agent actually experiences, mapped from the exit
// codes this program already defines rather than from a taxonomy invented for
// the spool.
const (
	// spoolOK is ExitOK.
	spoolOK = "ok"
	// spoolUsage is a refusal the caller can fix on its own side: the command,
	// a flag or an argument was wrong, or the credential was.
	spoolUsage = "usage"
	// spoolState is a refusal about the world: the document is not there, the
	// revision moved, or a local file could not be read or written. The
	// command was well formed and the answer will not change by retyping it.
	spoolState = "state"
	// spoolTransport is a failure to get an answer at all.
	spoolTransport = "transport"
	// spoolInternal is a failure this program never classified.
	spoolInternal = "internal"
)

// The closed vocabulary of error classes. Each one is an exit code this
// program documents, named rather than numbered so that a report reads.
const (
	spoolClassNotFound     = "not-found"
	spoolClassConflict     = "conflict"
	spoolClassAuth         = "auth"
	spoolClassInvalid      = "invalid"
	spoolClassLocalIO      = "local-io"
	spoolClassUnavailable  = "unavailable"
	spoolClassUnclassified = "unclassified"
)

// spoolErrClass names why an invocation failed, from the exit codes this
// program defines. It is deliberately not the error message: a message quotes
// the document id, the path or the search query that produced it, and the
// record is shipped to another host.
func spoolErrClass(code int) string {
	switch code {
	case ExitOK:
		return ""
	case ExitNotFound:
		return spoolClassNotFound
	case ExitConflict:
		return spoolClassConflict
	case ExitAuth:
		return spoolClassAuth
	case ExitInvalid:
		return spoolClassInvalid
	case ExitIO:
		return spoolClassLocalIO
	case ExitUnavailable:
		return spoolClassUnavailable
	}
	return spoolClassUnclassified
}

// spoolOutcome maps an error class onto the five classes an agent experiences.
//
// local-io is state rather than usage on purpose: the command was well formed
// and the filesystem refused it, which is a fact about the world the caller
// has to go and change, not a word it can retype. unclassified is the one
// class this program cannot map honestly, because ExitUnavailable is both
// "the server did not answer" and the default for an error nothing classified;
// only the second is recorded as unclassified, and only Run can tell them
// apart, which is why it is Run that names the class.
func spoolOutcome(class string) string {
	switch class {
	case "":
		return spoolOK
	case spoolClassInvalid, spoolClassAuth:
		return spoolUsage
	case spoolClassNotFound, spoolClassConflict, spoolClassLocalIO:
		return spoolState
	case spoolClassUnavailable:
		return spoolTransport
	default:
		return spoolInternal
	}
}

// spoolEnabled reports whether invocations are recorded. Off unless someone
// asked for it, in the same way and by the same switches as the MCP producers
// that share this spool: create the file "enabled" in the spool directory, or
// set JOCASTA_FRICTION=1, or set TOOLFEEDBACK=1 to move every tool together.
// Either variable set to 0 turns it off again.
func spoolEnabled() bool {
	for _, name := range []string{"JOCASTA_FRICTION", "TOOLFEEDBACK"} {
		switch os.Getenv(name) {
		case "1":
			return true
		case "0":
			return false
		}
	}
	dir := spoolDir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "enabled"))
	return err == nil
}

// spoolDir is where the spool files live, one directory per tool and one file
// per host per day. The precedence is the one the reader and the other
// producers use, so that pointing TOOLFEEDBACK_DIR at a scratch directory
// moves the whole spool at once - which is how the tests keep out of the
// operator's real one.
func spoolDir() string {
	for _, name := range []string{"JOCASTA_FRICTION_DIR", "TOOLFEEDBACK_DIR"} {
		if dir := os.Getenv(name); dir != "" {
			return dir
		}
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "toolfeedback")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "toolfeedback")
}

// spoolIdent keeps an identifier that came from the environment to the
// characters an identifier is made of, so that a spool line stays one line
// whatever an environment held, and bounds its length.
func spoolIdent(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > spoolMaxIdentLen {
		value = value[:spoolMaxIdentLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return -1
		}
	}, value)
}

// spoolSession is the agent session this invocation belongs to, as the harness
// exported it. A harness that exports none leaves the field empty rather than
// costing the record.
func spoolSession() string {
	for _, name := range []string{
		"TOOLFEEDBACK_SESSION",
		"JOCASTA_SESSION_ID",
		"CLAUDE_CODE_SESSION_ID",
		"CODEX_SESSION_ID",
		"OPENCODE_SESSION_ID",
	} {
		if value := spoolIdent(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// spoolClient is the harness that ran this command. The reason the spool is
// written by the binary rather than derived from Claude Code's transcripts is
// that a call made from Codex or OpenCode leaves no transcript to derive it
// from, and those are exactly the harnesses that read the most stale
// documentation, so a transcript-derived report would under-count them.
func spoolClient() string {
	if value := spoolIdent(os.Getenv("TOOLFEEDBACK_CLIENT")); value != "" {
		return value
	}
	for _, probe := range []struct {
		name      string
		variables []string
	}{
		{"claude-code", []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}},
		{"codex", []string{"CODEX_HOME", "CODEX_SANDBOX"}},
		{"opencode", []string{"OPENCODE", "OPENCODE_BIN_PATH"}},
	} {
		for _, variable := range probe.variables {
			if os.Getenv(variable) != "" {
				return probe.name
			}
		}
	}
	return ""
}

// spoolHost is this machine's short name.
func spoolHost() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	if index := strings.IndexByte(host, '.'); index > 0 {
		host = host[:index]
	}
	return host
}

// spoolProc tells two invocations apart when they share a session and a
// timestamp, which is what a shell pipeline of two calls produces.
func spoolProc() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return ""
	}
	return hex.EncodeToString(buffer)
}

// spoolVersion is which build made the call, so that a failure rate before a
// fix can be compared with the rate after it.
func spoolVersion() string {
	switch buildinfo.Commit {
	case "", "none", "unknown":
		// A build made outside a checkout knows its version and not its
		// commit; saying "unknown" in every record is noise, not information.
		return buildinfo.Version
	}
	commit := buildinfo.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return buildinfo.Version + "/" + commit
}

// spoolWords walks a command line the way parse does - skipping the value of a
// non-boolean flag, so that "--message get" is a message and not the verb -
// and answers the positional words and the flag keys it found. Scanning stops
// at a bare "--", because everything after it is the caller's own text.
//
// A flag key is recorded only when the parser's own flagset declares it, and
// otherwise as spoolUndeclaredFlag. Shape is not enough: "--sk-live-0xDEADBEEF"
// is shaped exactly like a flag, and a caller that mistypes a value into flag
// position would otherwise put it in a record that is shipped. An option this
// program does not have is still worth counting, so the fact is recorded and
// the word is not.
func spoolWords(args []string) (positional []string, keys []string) {
	var options options
	flags := newFlagSet(&options)
	seen := map[string]bool{}
	for index := 0; index < len(args); index++ {
		word := args[index]
		if word == "--" {
			break
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			positional = append(positional, word)
			continue
		}
		name := strings.TrimLeft(strings.SplitN(word, "=", 2)[0], "-")
		declared := flags.Lookup(name)
		key := spoolUndeclaredFlag
		if declared != nil && len(name) <= spoolMaxKeyLen {
			key = name
		} else if name == "help" || name == "h" {
			// Help is answered before the flagset is consulted, so it is not
			// registered there and is still a name this program publishes.
			key = name
		}
		if !seen[key] && len(keys) < spoolMaxArgs {
			seen[key] = true
			keys = append(keys, key)
		}
		// A non-boolean flag takes the next word as its value, and that word
		// is neither a verb nor a flag.
		if declared != nil && !strings.Contains(word, "=") {
			if boolean, ok := declared.Value.(interface{ IsBoolFlag() bool }); !ok || !boolean.IsBoolFlag() {
				index++
			}
		}
	}
	return positional, keys
}

// spoolVerbPath is the verb this invocation ran, as a full path: "jocasta
// search", not "jocasta".
//
// The word is recorded only when it names a help topic this program publishes,
// which makes the help registry the spool's whole vocabulary and is what keeps
// operands and mistyped values out: "jocasta get PROJECT/plans/secret.md"
// records the verb and never the reference. A verb nobody documents records as
// the bare program name, which is a signal rather than a loss.
func spoolVerbPath(args []string) string {
	positional, _ := spoolWords(args)
	// --version and -v name the version command; Run rewrites them before
	// parsing and the record says the same thing it does.
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-v") {
		return spoolSource + " version"
	}
	if len(positional) == 0 {
		return spoolSource
	}
	verb := positional[0]
	if verb != "help" && topicFor(verb) == nil {
		return spoolSource
	}
	return spoolSource + " " + verb
}

// spoolArgKeys is the ordered set of flag names this command line carried,
// with every value removed.
func spoolArgKeys(args []string) []string {
	_, keys := spoolWords(args)
	return keys
}

// newSpoolRecord builds the record for one finished invocation. It is separate
// from writing it so that a test can assert what a command line becomes
// without going near a file.
func newSpoolRecord(args []string, code int, class string, elapsed time.Duration) spoolRecord {
	return spoolRecord{
		TS:      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Host:    spoolHost(),
		Session: spoolSession(),
		Proc:    spoolProc(),
		Client:  spoolClient(),
		Ver:     spoolVersion(),
		// One process is one call, so the sequence is always the first. The
		// field is kept because the reader orders on it and every other
		// producer writes it.
		Seq:     1,
		Tool:    spoolVerbPath(args),
		Args:    spoolArgKeys(args),
		Outcome: spoolOutcome(class),
		OK:      class == "",
		Err:     class,
		Exit:    code,
		Ms:      elapsed.Milliseconds(),
	}
}

// writeSpoolRecord appends one record. Every failure is dropped in silence;
// see the file comment for why that is the correct behaviour here.
func writeSpoolRecord(record spoolRecord) {
	dir := spoolDir()
	if dir == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil || len(line) > spoolRecordLimit {
		return
	}
	host := record.Host
	if host == "" {
		host = "unknown"
	}
	// One directory per tool, because a host name contains hyphens and so does
	// a tool name: with both in one file name there is no way to tell where
	// the first ends and the second begins.
	dir = filepath.Join(dir, spoolSource)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, host+"-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	// One O_APPEND write of a short line, so that several processes spooling
	// at once interleave whole lines rather than fragments. No fsync: a
	// measurement is not worth a disk flush on every command.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
	_ = file.Close()
}

// spoolInvocation records one finished invocation, if the spool is on.
func spoolInvocation(args []string, code int, class string, elapsed time.Duration) {
	if !spoolEnabled() {
		return
	}
	writeSpoolRecord(newSpoolRecord(args, code, class, elapsed))
}
