package store

import (
	"bufio"
	"context"
	"fmt"
	"github.com/iryzhkov/jocasta/pkg/api"
	"os"
	"os/exec"
	"testing"
)

func TestAcknowledgedWriteSurvivesSIGKILL(t *testing.T) {
	if os.Getenv("JOCASTA_CRASH_CHILD") == "1" {
		s, e := Open(Config{Path: os.Getenv("JOCASTA_CRASH_DB"), MinFreeBytes: 1})
		if e != nil {
			panic(e)
		}
		content := "acknowledged\r\nπ\n"
		r, e := s.Mutate(context.Background(), "crash-host", "durable-request", api.Mutation{Operation: "create", Path: "crash/ack.md", Content: &content})
		if e != nil {
			panic(e)
		}
		fmt.Println(r.Document.ID)
		// Intentionally remain alive with a WAL connection open; parent kills us after acknowledgment.
		select {}
	}
	path := t.TempDir() + "/crash.db"
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcknowledgedWriteSurvivesSIGKILL$")
	cmd.Env = append(os.Environ(), "JOCASTA_CRASH_CHILD=1", "JOCASTA_CRASH_DB="+path)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	cmd.Stderr = os.Stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("child did not acknowledge write")
	}
	id := scanner.Text()
	if len(id) != 32 {
		t.Fatalf("unexpected acknowledgment %q", id)
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e == nil {
		t.Fatal("child was not killed")
	}
	s, e := Open(Config{Path: path, MinFreeBytes: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.Get(context.Background(), "", id, 0)
	if e != nil || r.Document.Content != "acknowledged\r\nπ\n" || r.Document.Revision != 1 {
		t.Fatalf("recovery %+v %v", r, e)
	}
	content := r.Document.Content
	replay, e := s.Mutate(context.Background(), "crash-host", "durable-request", api.Mutation{Operation: "create", Path: "crash/ack.md", Content: &content})
	if e != nil || replay.Document.ID != id {
		t.Fatalf("receipt recovery %+v %v", replay, e)
	}
	p, e := s.Page(context.Background(), api.Filter{Query: "acknowledged"}, true)
	if e != nil || len(p.Documents) != 1 {
		t.Fatalf("search recovery %+v %v", p, e)
	}
}
