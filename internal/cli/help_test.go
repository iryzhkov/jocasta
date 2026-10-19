package cli

import (
	"bytes"
	"context"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func helpRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut, "test")
	return code, out.String(), errOut.String()
}

// dispatchedVerbs reads the verbs out of execute's dispatch switch instead of
// keeping a second list beside it, so a verb added to the switch without a help
// page fails the test that uses this.
func dispatchedVerbs(t *testing.T) []string {
	t.Helper()
	file, e := parser.ParseFile(token.NewFileSet(), "cli.go", nil, 0)
	if e != nil {
		t.Fatal(e)
	}
	var verbs []string
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "execute" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || len(verbs) > 0 {
				return true
			}
			if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "cmd" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, e := strconv.Unquote(lit.Value)
					if e == nil {
						verbs = append(verbs, v)
					}
				}
			}
			return false
		})
	}
	if len(verbs) == 0 {
		t.Fatal("no dispatch switch on cmd found in cli.go; this test reads the verbs from it")
	}
	return verbs
}

func TestEveryDispatchedVerbHasAPage(t *testing.T) {
	verbs := dispatchedVerbs(t)
	if len(verbs) < 12 {
		t.Fatalf("expected the dispatch switch to name every verb, got %v", verbs)
	}
	known := map[string]bool{"version": true}
	for _, v := range verbs {
		known[v] = true
		if topicFor(v) == nil {
			t.Errorf("command %q has no help page", v)
		}
	}
	if topicFor("version") == nil {
		t.Error("command \"version\" has no help page")
	}
	for _, tp := range helpTopics {
		if !known[tp.verb] {
			t.Errorf("help page %q documents no command", tp.verb)
		}
	}
}

func TestEveryPageReachesStdoutWithExitZero(t *testing.T) {
	for _, tp := range helpTopics {
		var pages []string
		for _, args := range [][]string{{tp.verb, "--help"}, {tp.verb, "-h"}, {"help", tp.verb}} {
			code, out, errOut := helpRun(t, args...)
			if code != ExitOK {
				t.Fatalf("%v: exit %d, stderr %q", args, code, errOut)
			}
			if errOut != "" {
				t.Errorf("%v: wrote %q to stderr", args, errOut)
			}
			if !strings.HasPrefix(out, "jocasta "+tp.verb+" ") {
				t.Errorf("%v: page does not open by naming the command: %q", args, out)
			}
			for _, section := range []string{"\nUsage\n", "\nFlags\n", "\nGuarantees\n", "\nExit codes\n"} {
				if !strings.Contains(out, section) {
					t.Errorf("%v: page has no %q section", args, strings.TrimSpace(section))
				}
			}
			pages = append(pages, out)
		}
		if pages[0] != pages[1] || pages[1] != pages[2] {
			t.Errorf("%s: the three spellings print different pages", tp.verb)
		}
	}
	code, out, errOut := helpRun(t, "help", "nosuchverb")
	if code != ExitInvalid || out != "" || !strings.Contains(errOut, "no help for") {
		t.Fatalf("unknown topic: %d %q %q", code, out, errOut)
	}
}

// The pages must work on a host with no Jocasta credentials and no
// configuration file at all.
func TestHelpNeedsNoConfigurationOrCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	absent := filepath.Join(dir, "absent.json")
	for _, args := range [][]string{{"put", "--help"}, {"help", "put"}, {"put", "-h", "--config", absent}, {"help"}, {"--help"}} {
		code, out, errOut := helpRun(t, args...)
		if code != ExitOK || out == "" || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
}

// Flags cannot be attributed to verbs mechanically, because the CLI builds one
// global flagset before it chooses a verb. What is asserted mechanically is
// everything that attribution does not decide: every flag a page claims is a
// flag the parser really registers, every registered flag appears on some
// page, and each page prints the flag's own description and default, taken
// from the flagset rather than restated.
func TestPagesDocumentRegisteredFlagsWithTheirDefaults(t *testing.T) {
	var o options
	registered := map[string]*flag.Flag{}
	newFlagSet(&o).VisitAll(func(f *flag.Flag) { registered[f.Name] = f })
	documented := map[string]bool{}
	for _, tp := range helpTopics {
		_, page, _ := helpRun(t, tp.verb, "--help")
		seen := map[string]bool{}
		for _, name := range tp.flags {
			f := registered[name]
			if f == nil {
				t.Fatalf("the %s page claims --%s, which the parser does not register", tp.verb, name)
			}
			if seen[name] {
				t.Errorf("the %s page lists --%s twice", tp.verb, name)
			}
			seen[name] = true
			documented[name] = true
			if !strings.Contains(page, "\n  --"+name+" ") {
				t.Errorf("the %s page does not print --%s", tp.verb, name)
			}
			if !strings.Contains(page, f.Usage) {
				t.Errorf("the %s page does not print the description of --%s", tp.verb, name)
			}
			d := f.DefValue
			if d == "" {
				d = `""`
			}
			if !strings.Contains(page, "(default "+d+")") {
				t.Errorf("the %s page does not print the default of --%s", tp.verb, name)
			}
		}
	}
	for name := range registered {
		if !documented[name] {
			t.Errorf("--%s is registered but appears on no page", name)
		}
	}
}

func TestPageExitCodesAreRealAndSpecific(t *testing.T) {
	valid := map[int]bool{ExitOK: true, ExitNotFound: true, ExitConflict: true, ExitAuth: true, ExitUnavailable: true, ExitInvalid: true, ExitIO: true}
	for _, tp := range helpTopics {
		if len(tp.exits) == 0 || tp.exits[0].code != ExitOK {
			t.Fatalf("the %s page does not start its exit codes with success", tp.verb)
		}
		seen := map[int]bool{}
		for _, x := range tp.exits {
			if !valid[x.code] {
				t.Errorf("the %s page names exit %d, which the CLI does not define", tp.verb, x.code)
			}
			if seen[x.code] {
				t.Errorf("the %s page names exit %d twice", tp.verb, x.code)
			}
			seen[x.code] = true
			if x.meaning == "" {
				t.Errorf("the %s page gives exit %d no meaning", tp.verb, x.code)
			}
		}
		if tp.verb != "version" && !seen[ExitInvalid] {
			t.Errorf("the %s page does not say when it refuses input", tp.verb)
		}
	}
}

// A mutating verb's page must teach the guard contract, not restate the flag:
// what the expected revision asserts, what a pinned reference does, what a
// conflict means, and what is true of the document when a write is refused.
func TestMutatingPagesTeachTheGuardContract(t *testing.T) {
	for _, verb := range []string{"put", "mv", "archive", "unarchive", "restore"} {
		tp := topicFor(verb)
		if tp == nil || !tp.guard {
			t.Fatalf("%s is a mutating verb without the guard contract", verb)
		}
		_, page, _ := helpRun(t, verb, "--help")
		for _, want := range []string{
			"--if-revision",
			"jocasta:ID@REVISION",
			"expected current revision",
			"exit 3",
			"unchanged",
			"jocasta get jocasta:ID@CURRENT",
			"--request-id",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the %s page does not teach %q", verb, want)
			}
		}
	}
}

func TestVersionFlagIsTheVersionCommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		code, out, errOut := helpRun(t, args...)
		if code != ExitOK || strings.TrimSpace(out) != "test" || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	for _, args := range [][]string{{"version", "--json"}, {"--version", "--json"}, {"-v", "--json"}} {
		code, out, errOut := helpRun(t, args...)
		if code != ExitOK || !strings.Contains(out, `"version":"test"`) || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	// Anywhere else it is still an unknown flag, and the refusal names the
	// accepted form rather than leaving the caller to guess it.
	code, _, errOut := helpRun(t, "get", "p/a.md", "--version")
	if code != ExitInvalid || !strings.Contains(errOut, "jocasta version") {
		t.Fatalf("%d %q", code, errOut)
	}
}

// No help path can perform a request, by construction: the file that renders
// the pages imports neither the HTTP package nor the client, so there is
// nothing there to make a request with. The server below would fail the test
// if a page reached it anyway.
func TestNoHelpPathPerformsARequest(t *testing.T) {
	file, e := parser.ParseFile(token.NewFileSet(), "help.go", nil, parser.ImportsOnly)
	if e != nil {
		t.Fatal(e)
	}
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == "net/http" || strings.HasSuffix(p, "internal/client") {
			t.Errorf("help.go imports %q, so a help page could make a request", p)
		}
	}
	conf := testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a help page performed a request: %s %s", r.Method, r.URL.Path)
	}))
	for _, tp := range helpTopics {
		for _, args := range [][]string{{tp.verb, "--help"}, {tp.verb, "-h"}, {"help", tp.verb}} {
			if code, _, errOut := helpRun(t, append(args, "--config", conf)...); code != ExitOK || errOut != "" {
				t.Fatalf("%v: %d %q", args, code, errOut)
			}
		}
	}
}

// exitMeaning returns what a page says exit code means for its verb, or the
// empty string when the page does not list that code at all.
func exitMeaning(tp helpTopic, code int) string {
	for _, x := range tp.exits {
		if x.code == code {
			return x.meaning
		}
	}
	return ""
}

// The exit-code lists are the one part of a page that is neither rendered from
// the flagset nor derived from the dispatch switch, so this test and the two
// below provoke the codes instead of believing them.
//
// Loading the configuration file is the first thing execute does, before it
// looks at any argument of the verb, so a host that has never been configured
// gets exit 7 from every verb but version. That is the first refusal such a
// host meets, and every page whose verb returns it must say so.
func TestPagesDocumentTheConfigurationExitCode(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "no-such-config.json")
	for _, tp := range helpTopics {
		code, _, errOut := helpRun(t, tp.verb, "--config", absent)
		documented := exitMeaning(tp, ExitIO)
		if tp.verb == "version" {
			if code != ExitOK {
				t.Errorf("version reads configuration after all: exit %d, stderr %q", code, errOut)
			}
			if documented != "" {
				t.Errorf("the version page claims exit 7, which that verb cannot return")
			}
			continue
		}
		if code != ExitIO {
			t.Errorf("%s with a configuration file that does not exist: exit %d, stderr %q; expected exit 7", tp.verb, code, errOut)
			continue
		}
		if documented == "" {
			t.Errorf("%s returns exit 7 for a missing configuration file and its page does not list exit 7", tp.verb)
			continue
		}
		// The meaning is checked, not only the code: put and get listed exit 7
		// for --file and --output alone, which reads as though a missing
		// configuration file were something else.
		if !strings.Contains(documented, "configuration file") {
			t.Errorf("the %s page lists exit 7 without naming the configuration file: %q", tp.verb, documented)
		}
	}
	// --dry-run is the one path that reads no configuration, which is what lets
	// the import page promise that it makes no request.
	if code, _, errOut := helpRun(t, "import", t.TempDir(), "--dry-run", "--migration-id", "m", "--config", absent); code != ExitOK {
		t.Errorf("import --dry-run with a missing configuration file: exit %d, stderr %q; expected exit 0", code, errOut)
	}
}

// conflictFamily reports, for every function declared in the store's query
// file, whether it can return a code the CLI maps to exit 3. It is read out of
// the store rather than restated here, because a page that claims a conflict
// the store cannot produce is exactly the error this guards.
func conflictFamily(t *testing.T) map[string]bool {
	t.Helper()
	file, e := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "store", "query.go"), nil, 0)
	if e != nil {
		t.Fatal(e)
	}
	codes := map[string]bool{"conflict": true, "cursor_expired": true, "idempotency_conflict": true, "path_exists": true}
	funcs := map[string]bool{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, seen := funcs[fn.Name.Name]; !seen {
			funcs[fn.Name.Name] = false
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, e := strconv.Unquote(lit.Value); e == nil && codes[v] {
				funcs[fn.Name.Name] = true
			}
			return true
		})
	}
	return funcs
}

// A page may claim exit 3 only where a conflict can really arise: a guarded
// mutation, the import's re-scan, or an enumeration whose cursor the store
// expires. Which enumerations those are is decided by the store function that
// serves the verb, not by this test: ls and search page through Store.Page,
// which compares cursor generations, and history pages through Store.History,
// which anchors to a maximum revision instead and so cannot expire a cursor.
func TestPagesClaimConflictOnlyWhereItCanHappen(t *testing.T) {
	servedBy := map[string]string{"ls": "Page", "search": "Page", "history": "History"}
	funcs := conflictFamily(t)
	for _, tp := range helpTopics {
		want := tp.guard || tp.verb == "import"
		if fn, paging := servedBy[tp.verb]; paging {
			conflicts, declared := funcs[fn]
			if !declared {
				t.Fatalf("internal/store/query.go declares no %s; this test reads cursor expiry from it", fn)
			}
			want = conflicts
		}
		meaning := exitMeaning(tp, ExitConflict)
		if want && meaning == "" {
			t.Errorf("the %s page does not list exit 3, which that verb can return", tp.verb)
		}
		if !want && meaning != "" {
			t.Errorf("the %s page claims exit 3 (%q), which that verb cannot return", tp.verb, meaning)
		}
	}
}

// verbInvocations is one minimal invocation of every verb that talks to the
// server. The table restates no rule: an invocation the parser or the verb
// would refuse locally never reaches the stub below, and the assertions then
// fail with exit 6 rather than passing quietly.
func verbInvocations(t *testing.T) map[string][]string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "content.md")
	if e := os.WriteFile(file, []byte("body\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	return map[string][]string{
		"put":       {"put", "p/a.md", "--file", file, "--if-revision", "1"},
		"get":       {"get", "p/a.md"},
		"exists":    {"exists", "p/a.md"},
		"ls":        {"ls"},
		"search":    {"search", "query"},
		"history":   {"history", "p/a.md"},
		"diff":      {"diff", "p/a.md", "--from", "1", "--to", "2"},
		"mv":        {"mv", "p/a.md", "p/b.md", "--if-revision", "1"},
		"archive":   {"archive", "p/a.md", "--if-revision", "1"},
		"unarchive": {"unarchive", "p/a.md", "--if-revision", "1"},
		"restore":   {"restore", "p/a.md", "--revision", "1", "--if-revision", "2"},
	}
}

// Exit 4 and exit 5 are claimed by every page whose verb reaches the server,
// so both are provoked here: a stub that refuses the token, and a stub that
// fails. A verb that makes no request must claim neither.
func TestPagesClaimTheServerFailuresTheyReturn(t *testing.T) {
	stub := func(status int, body string) string {
		return testConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	cases := []struct {
		conf string
		want int
	}{
		{stub(http.StatusUnauthorized, `{"version":"jocasta/v1","error":{"code":"auth","message":"authentication failed"}}`), ExitAuth},
		{stub(http.StatusInternalServerError, "the server failed"), ExitUnavailable},
	}
	invocations := verbInvocations(t)
	for _, tp := range helpTopics {
		args, reaches := invocations[tp.verb]
		if !reaches {
			continue
		}
		for _, c := range cases {
			code, _, errOut := helpRun(t, append(append([]string{}, args...), "--config", c.conf)...)
			if code != c.want {
				t.Errorf("%v: exit %d, stderr %q; expected exit %d", args, code, errOut, c.want)
				continue
			}
			if exitMeaning(tp, c.want) == "" {
				t.Errorf("%s returns exit %d and its page does not list it", tp.verb, c.want)
			}
		}
	}
	version := topicFor("version")
	if exitMeaning(*version, ExitAuth) != "" || exitMeaning(*version, ExitUnavailable) != "" {
		t.Error("the version page claims a server failure, but the verb makes no request")
	}
}
