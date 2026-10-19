package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// spoolSecret is the argument value every leak test drives through the CLI. It
// is shaped like a credential on purpose: if any part of the spool ever copies
// an argument value instead of a name, this string is what shows up in the
// record, and the record is shipped to another host.
const spoolSecret = "sk-live-0xDEADBEEFCAFE-SECRET"

// useSpool points the spool at a scratch directory and turns it on, and
// answers that directory. Every test in this file calls it: a test must never
// append to the operator's real spool under ~/.local/share/toolfeedback.
func useSpool(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TOOLFEEDBACK_DIR", dir)
	t.Setenv("JOCASTA_FRICTION_DIR", "")
	t.Setenv("JOCASTA_FRICTION", "1")
	return dir
}

// noConfig is a configuration file that does not exist, so that a test command
// fails locally instead of reaching whatever server this machine is configured
// for. Every command line in this file names it.
func noConfig(t *testing.T) []string {
	t.Helper()
	return []string{"--config", filepath.Join(t.TempDir(), "absent.json")}
}

// runSpooled drives one command line through the same entry point the binary
// uses and answers its exit code and both output streams.
func runSpooled(args []string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := RunSpooled(context.Background(), args, strings.NewReader(""), &out, &errOut, "test")
	return code, out.String(), errOut.String()
}

func spoolPaths(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob the spool: %v", err)
	}
	return matches
}

// spoolBytes is everything the spool holds, for the assertion that a value
// appears nowhere in it at all - not in a field this test knows to look at,
// and not in one added later either.
func spoolBytes(t *testing.T, dir string) string {
	t.Helper()
	var all strings.Builder
	for _, path := range spoolPaths(t, dir) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		all.Write(content)
	}
	return all.String()
}

func spoolLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimRight(spoolBytes(t, dir), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record is not JSON: %v (%q)", err, line)
		}
		records = append(records, record)
	}
	return records
}

func recordString(t *testing.T, record map[string]any, key string) string {
	t.Helper()
	value, present := record[key]
	if !present {
		t.Fatalf("record has no %q: %v", key, record)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("record %q is %T, want a string", key, value)
	}
	return text
}

func recordKeys(record map[string]any) []string {
	var keys []string
	// args is omitted entirely when a command line carried no flag, which is
	// what the reader expects.
	if recorded, present := record["args"].([]any); present {
		for _, value := range recorded {
			keys = append(keys, value.(string))
		}
	}
	return keys
}

// A successful invocation writes exactly one record, and its tool is the full
// verb path rather than the binary name.
func TestSpoolRecordsOneSuccessWithTheFullVerbPath(t *testing.T) {
	dir := useSpool(t)

	code, out, errOut := runSpooled([]string{"get", "--help"})
	if code != ExitOK {
		t.Fatalf("exit code %d, want %d (stderr %q)", code, ExitOK, errOut)
	}
	if out == "" {
		t.Fatal("the help page printed nothing")
	}

	records := spoolLines(t, dir)
	if len(records) != 1 {
		t.Fatalf("%d records, want exactly 1: %v", len(records), records)
	}
	record := records[0]
	if got := recordString(t, record, "tool"); got != "jocasta get" {
		t.Errorf("tool is %q, want the full verb path %q", got, "jocasta get")
	}
	if got := recordString(t, record, "outcome"); got != spoolOK {
		t.Errorf("outcome is %q, want %q", got, spoolOK)
	}
	if ok, _ := record["ok"].(bool); !ok {
		t.Errorf("ok is %v, want true", record["ok"])
	}
	for _, key := range []string{"ts", "host", "session", "proc", "seq", "tool", "outcome", "ok", "exit", "ms"} {
		if _, present := record[key]; !present {
			t.Errorf("record has no %q: %v", key, record)
		}
	}
}

// A refusal writes one record whose outcome is the refusal class, and the exit
// code the caller sees is the one Run returns without the spool.
func TestSpoolRecordsARefusalWithoutChangingTheExitCode(t *testing.T) {
	dir := useSpool(t)

	args := append([]string{"frobnicate"}, noConfig(t)...)
	code, _, errOut := runSpooled(args)
	if code != ExitInvalid {
		t.Fatalf("exit code %d, want %d", code, ExitInvalid)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Fatalf("stderr %q does not carry the refusal", errOut)
	}
	// Run is the path the binary took before the spool existed. It writes no
	// record, so the comparison does not cost the test a second line.
	var out, plain bytes.Buffer
	if want := Run(context.Background(), args, strings.NewReader(""), &out, &plain, "test"); want != code {
		t.Fatalf("RunSpooled returned %d, Run returns %d", code, want)
	}

	records := spoolLines(t, dir)
	if len(records) != 1 {
		t.Fatalf("%d records, want exactly 1: %v", len(records), records)
	}
	record := records[0]
	if got := recordString(t, record, "outcome"); got != spoolUsage {
		t.Errorf("outcome is %q, want %q: a command that does not exist is a refusal the caller can fix", got, spoolUsage)
	}
	if got := recordString(t, record, "err"); got != spoolClassInvalid {
		t.Errorf("err is %q, want %q", got, spoolClassInvalid)
	}
	if ok, _ := record["ok"].(bool); ok {
		t.Error("ok is true for a refusal")
	}
	// The word the caller typed where a verb should stand names no help topic,
	// so the path stops at the program. The word is never echoed.
	if got := recordString(t, record, "tool"); got != "jocasta" {
		t.Errorf("tool is %q, want %q", got, "jocasta")
	}
	if strings.Contains(spoolBytes(t, dir), "frobnicate") {
		t.Error("the spool echoed the word the caller typed where a verb should stand")
	}
}

// The test that matters most. A command line whose flag values, positional
// arguments and trailing text all carry a recognisable secret produces a
// record in which the secret appears nowhere at all. A spool that leaks
// argument values is worse than no spool, because these records are shipped.
func TestSpoolNeverRecordsAnArgumentValue(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		tool string
		keys []string
	}{
		{
			name: "secret in a document reference, an output path and a message",
			args: []string{"put", "PROJECT/handoffs/" + spoolSecret + ".md",
				"--message", "deploy with " + spoolSecret,
				"--output", "/tmp/" + spoolSecret + "/out.md", "--create"},
			tool: "jocasta put",
			keys: []string{"config", "message", "output", "create"},
		},
		{
			name: "secret in an exact reference and a search query",
			args: []string{"search", spoolSecret, "--id", "jocasta:" + spoolSecret + "@7", "--json"},
			tool: "jocasta search",
			keys: []string{"config", "id", "json"},
		},
		{
			name: "secret joined to the flag with an equals sign",
			args: []string{"ls", "--prefix=" + spoolSecret},
			tool: "jocasta ls",
			keys: []string{"config", "prefix"},
		},
		{
			name: "secret standing where a command should",
			args: []string{spoolSecret, "--json"},
			tool: "jocasta",
			keys: []string{"config", "json"},
		},
		{
			name: "secret standing where a flag name should",
			args: []string{"get", "--" + spoolSecret},
			tool: "jocasta get",
			keys: []string{"config", spoolUndeclaredFlag},
		},
		{
			name: "secret after a bare double dash",
			args: []string{"get", "--", spoolSecret, "--token", spoolSecret},
			tool: "jocasta get",
			keys: []string{"config"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := useSpool(t)
			// --config comes first so that it is recorded first whatever the
			// case appends, and so that no command reaches a real server.
			runSpooled(append(noConfig(t), testCase.args...))

			content := spoolBytes(t, dir)
			if content == "" {
				t.Fatal("no record was written")
			}
			if strings.Contains(content, spoolSecret) {
				t.Fatalf("the secret reached the spool: %s", content)
			}
			// The same assertion for the pieces a partial copy would leave
			// behind: a path fragment, or the value with its prefix stripped.
			for _, fragment := range []string{"DEADBEEF", "sk-live", "/tmp/", "PROJECT/", "jocasta:"} {
				if strings.Contains(content, fragment) {
					t.Fatalf("the spool carries %q from the command line: %s", fragment, content)
				}
			}

			records := spoolLines(t, dir)
			if len(records) != 1 {
				t.Fatalf("%d records, want exactly 1", len(records))
			}
			if got := recordString(t, records[0], "tool"); got != testCase.tool {
				t.Errorf("tool is %q, want %q", got, testCase.tool)
			}
			if got := recordKeys(records[0]); strings.Join(got, ",") != strings.Join(testCase.keys, ",") {
				t.Errorf("args are %v, want %v", got, testCase.keys)
			}
		})
	}
}

// A flag the parser declares is recorded by name; anything else that stood
// where a flag should is recorded as the fact that it was there. Deriving the
// allowlist from the parser's own flagset is what makes the two sets the same
// set, with nothing to keep in step by hand.
func TestSpoolRecordsDeclaredFlagNamesOnly(t *testing.T) {
	var declared options
	flags := newFlagSet(&declared)
	flags.VisitAll(func(f *flag.Flag) {
		if got := spoolArgKeys([]string{"get", "--" + f.Name + "=x"}); len(got) != 1 || got[0] != f.Name {
			t.Errorf("--%s records as %v, want it recorded by name", f.Name, got)
		}
	})
	for _, name := range []string{"totally-not-a-flag", spoolSecret, "notify-thread"} {
		if got := spoolArgKeys([]string{"get", "--" + name + "=x"}); len(got) != 1 || got[0] != spoolUndeclaredFlag {
			t.Errorf("--%s records as %v, want %q", name, got, spoolUndeclaredFlag)
		}
	}
	// A flag's value is never mistaken for the verb, and one "?" stands for
	// every undeclared word, so the record says an option this program does
	// not have was passed and never which one.
	args := []string{"--message", "get", "put", "--" + spoolSecret, "--totally-not-a-flag", "--json"}
	if got := spoolVerbPath(args); got != "jocasta put" {
		t.Errorf("verb path is %q, want %q", got, "jocasta put")
	}
	if want := []string{"message", spoolUndeclaredFlag, "json"}; strings.Join(spoolArgKeys(args), ",") != strings.Join(want, ",") {
		t.Errorf("args are %v, want %v", spoolArgKeys(args), want)
	}
}

// A spool that cannot be written changes neither the exit code nor a byte of
// either output stream. This is the one place in this program where silence is
// the correct behaviour.
func TestUnwritableSpoolChangesNothingTheCallerSees(t *testing.T) {
	args := append([]string{"frobnicate"}, noConfig(t)...)

	writable := useSpool(t)
	wantCode, wantOut, wantErr := runSpooled(args)
	if len(spoolPaths(t, writable)) != 1 {
		t.Fatalf("the writable run wrote %d spool files, want 1", len(spoolPaths(t, writable)))
	}

	// A regular file where the spool's parent directory must be. MkdirAll
	// fails with ENOTDIR on it for every user, including root, so this test
	// does not quietly pass by running as somebody who can write anywhere.
	blocked := t.TempDir()
	obstruction := filepath.Join(blocked, "not-a-directory")
	if err := os.WriteFile(obstruction, []byte(""), 0o600); err != nil {
		t.Fatalf("write the obstruction: %v", err)
	}
	t.Setenv("TOOLFEEDBACK_DIR", filepath.Join(obstruction, "spool"))
	t.Setenv("JOCASTA_FRICTION", "1")

	code, out, errOut := runSpooled(args)
	if code != wantCode {
		t.Errorf("exit code %d with an unwritable spool, %d with a writable one", code, wantCode)
	}
	if out != wantOut {
		t.Errorf("stdout changed:\n unwritable %q\n writable   %q", out, wantOut)
	}
	if errOut != wantErr {
		t.Errorf("stderr changed:\n unwritable %q\n writable   %q", errOut, wantErr)
	}
	if entries, err := os.ReadDir(blocked); err != nil || len(entries) != 1 {
		t.Errorf("the blocked directory holds %v (err %v), want only the obstruction", entries, err)
	}
}

// The file layout and the record shape are the ones homelab-cli's toolfeedback
// reader consumes: <dir>/<source>/<host>-<YYYY-MM-DD>.jsonl, one JSON object
// per line, with the keys it reads.
func TestSpoolMatchesTheReaderShape(t *testing.T) {
	dir := useSpool(t)
	runSpooled([]string{"get", "--help"})

	paths := spoolPaths(t, dir)
	if len(paths) != 1 {
		t.Fatalf("%d spool files, want 1: %v", len(paths), paths)
	}
	if source := filepath.Base(filepath.Dir(paths[0])); source != spoolSource {
		t.Errorf("the spool directory is %q, want %q", source, spoolSource)
	}
	// SPOOL_NAME_RE in homelab-cli/bin/toolfeedback.
	name := regexp.MustCompile(`^(?P<host>.+)-(?P<date>\d{4}-\d{2}-\d{2})\.jsonl$`)
	if !name.MatchString(filepath.Base(paths[0])) {
		t.Errorf("%q is not a spool file name the reader recognises", filepath.Base(paths[0]))
	}

	record := spoolLines(t, dir)[0]
	// event_time in the reader parses this after replacing Z with +00:00.
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", recordString(t, record, "ts")); err != nil {
		t.Errorf("ts %q is not the timestamp format the reader parses: %v", record["ts"], err)
	}
	for key, want := range map[string]string{
		"host": "string", "session": "string", "proc": "string", "tool": "string",
		"seq": "number", "ms": "number", "ok": "bool",
	} {
		value, present := record[key]
		if !present {
			t.Errorf("record has no %q, which the reader reads", key)
			continue
		}
		var got string
		switch value.(type) {
		case string:
			got = "string"
		case float64:
			got = "number"
		case bool:
			got = "bool"
		default:
			got = "other"
		}
		if got != want {
			t.Errorf("record %q is a %s, the reader wants a %s", key, got, want)
		}
	}
}

// Every exit code this program defines maps onto one of the five classes an
// agent experiences.
func TestSpoolOutcomeMapsTheDefinedExitCodes(t *testing.T) {
	for _, testCase := range []struct {
		code    int
		class   string
		outcome string
	}{
		{ExitOK, "", spoolOK},
		{ExitNotFound, spoolClassNotFound, spoolState},
		{ExitConflict, spoolClassConflict, spoolState},
		{ExitAuth, spoolClassAuth, spoolUsage},
		{ExitUnavailable, spoolClassUnavailable, spoolTransport},
		{ExitInvalid, spoolClassInvalid, spoolUsage},
		{ExitIO, spoolClassLocalIO, spoolState},
	} {
		if class := spoolErrClass(testCase.code); class != testCase.class {
			t.Errorf("exit %d classes as %q, want %q", testCase.code, class, testCase.class)
		}
		if outcome := spoolOutcome(testCase.class); outcome != testCase.outcome {
			t.Errorf("class %q is outcome %q, want %q", testCase.class, outcome, testCase.outcome)
		}
	}
	if outcome := spoolOutcome(spoolClassUnclassified); outcome != spoolInternal {
		t.Errorf("an unclassified failure is outcome %q, want %q", outcome, spoolInternal)
	}
}

// The spool is off unless it was asked for, in the same way as the MCP
// producers that share the directory.
func TestSpoolIsOffUnlessAskedFor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOOLFEEDBACK_DIR", dir)
	t.Setenv("JOCASTA_FRICTION", "")
	t.Setenv("TOOLFEEDBACK", "")
	if spoolEnabled() {
		t.Fatal("the spool is on in a directory with no enabled marker")
	}
	runSpooled([]string{"get", "--help"})
	if paths := spoolPaths(t, dir); len(paths) != 0 {
		t.Fatalf("the spool wrote %v while off", paths)
	}

	if err := os.WriteFile(filepath.Join(dir, "enabled"), nil, 0o600); err != nil {
		t.Fatalf("write the enabled marker: %v", err)
	}
	if !spoolEnabled() {
		t.Fatal("the enabled marker did not turn the spool on")
	}
	t.Setenv("TOOLFEEDBACK", "0")
	if spoolEnabled() {
		t.Fatal("TOOLFEEDBACK=0 did not turn the spool off")
	}
}
