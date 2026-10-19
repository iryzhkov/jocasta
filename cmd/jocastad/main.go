package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/iryzhkov/jocasta/internal/buildinfo"
	"github.com/iryzhkov/jocasta/internal/server"
	"github.com/iryzhkov/jocasta/internal/store"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func env(name, fallback string) string {
	if x := os.Getenv(name); x != "" {
		return x
	}
	return fallback
}
func number(name string, fallback int64) int64 {
	x := os.Getenv(name)
	if x == "" {
		return fallback
	}
	n, e := strconv.ParseInt(x, 10, 64)
	if e != nil || n < 1 {
		log.Fatalf("invalid %s", name)
	}
	return n
}
func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("%s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return
	}
	mode := "serve"
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "serve" || args[0] == "backup" || args[0] == "reindex" || args[0] == "archive-inactive") {
		mode = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("jocastad "+mode, flag.ExitOnError)
	db := fs.String("db", env("JOCASTA_DB", "/data/jocasta.db"), "local SQLite database")
	listen := fs.String("listen", env("JOCASTA_LISTEN", ":8080"), "HTTP listen address; restrict to TLS ingress")
	tokens := fs.String("tokens-file", env("JOCASTA_TOKENS_FILE", "/run/secrets/jocasta-tokens.json"), "JSON object mapping actors to bearer tokens")
	output := fs.String("output", "", "new backup database path")
	days := fs.Int("days", int(number("JOCASTA_ARCHIVE_AFTER_DAYS", 30)), "inactivity days before archival")
	fs.Parse(args)
	cfg := store.Config{Path: *db, MaxDocumentBytes: number("JOCASTA_MAX_DOCUMENT_BYTES", 16<<20), MaxStorageBytes: number("JOCASTA_MAX_STORAGE_BYTES", 10<<30), MinFreeBytes: number("JOCASTA_MIN_FREE_BYTES", 1<<30)}
	s, err := store.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	switch mode {
	case "backup":
		if *output == "" {
			log.Fatal("--output required")
		}
		if err = s.Backup(ctx, *output); err != nil {
			log.Fatal(err)
		}
		return
	case "reindex":
		if err = s.Reindex(ctx); err != nil {
			log.Fatal(err)
		}
		return
	case "archive-inactive":
		n, e := s.ArchiveInactive(ctx, *days)
		if e != nil {
			log.Fatal(e)
		}
		fmt.Printf("archived %d documents\n", n)
		return
	}
	if err = server.ValidateTokens(*tokens); err != nil {
		log.Fatal(err)
	}
	handler := server.New(s, server.Config{TokensFile: *tokens, MaxDocumentBytes: cfg.MaxDocumentBytes})
	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if _, e := s.ArchiveInactive(ctx, *days); e != nil && ctx.Err() == nil {
				log.Printf("inactivity archival failed: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	log.Printf("jocastad %s (%s) listening on %s", buildinfo.Version, buildinfo.Commit, *listen)
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
