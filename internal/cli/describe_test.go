package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDescribeNeedsNoConfigAndNeverSpools(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	if os.Getenv("HOME") != scratch {
		t.Fatal("scratch HOME required")
	}
	t.Setenv("JOCASTA_CONFIG", filepath.Join(scratch, "missing"))
	t.Setenv("JOCASTA_FRICTION", "1")
	t.Setenv("TOOLFEEDBACK_DIR", filepath.Join(scratch, "spool"))
	var out, errOut bytes.Buffer
	if code := RunSpooled(context.Background(), []string{"--describe"}, bytes.NewReader(nil), &out, &errOut, "test"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatal(errOut.String())
	}
	var data struct {
		Name, Summary, Help string
		Commands            []struct{ Name, Summary, Access string }
	}
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Name != "jocasta" || data.Help != "jocasta --help" || data.Summary == "" || len(data.Commands) != len(helpTopics)+1 {
		t.Fatalf("%+v", data)
	}
	for _, command := range data.Commands {
		want := "write"
		switch command.Name {
		case "ls", "search", "history", "version", "help":
			want = "read"
		}
		if command.Access != want || command.Summary == "" {
			t.Fatalf("%+v", command)
		}
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("discovery wrote scratch: %v %v", entries, err)
	}
}
