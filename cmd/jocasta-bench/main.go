// jocasta-bench creates an isolated synthetic corpus and reports reproducible store latency.
// Never point --db at a production database; --measure-only accepts only a synthetic corpus.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"
)

type latency struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	MaxMS float64 `json:"max_ms"`
}

func stats(values []float64) latency {
	sort.Float64s(values)
	return latency{values[len(values)/2], values[(len(values)*95-1)/100], values[len(values)-1]}
}
func main() {
	path := flag.String("db", "", "new isolated benchmark database path")
	count := flag.Int("documents", 10000, "number of documents")
	total := flag.Int64("bytes", 1<<30, "total source content bytes")
	samples := flag.Int("samples", 200, "query/read measurement count")
	measureOnly := flag.Bool("measure-only", false, "measure an existing synthetic benchmark database without rebuilding corpus")
	flag.Parse()
	if *path == "" || *count < 1 || *total < int64(*count) || *samples < 1 {
		fmt.Fprintln(os.Stderr, "--db and positive corpus/sample sizes required")
		os.Exit(2)
	}
	if _, e := os.Stat(*path); !*measureOnly && !os.IsNotExist(e) {
		fmt.Fprintln(os.Stderr, "database path must not exist")
		os.Exit(2)
	}
	s, e := store.Open(store.Config{Path: *path, MinFreeBytes: 10 << 30, MaxStorageBytes: *total*16 + 1<<30})
	if e != nil {
		panic(e)
	}
	defer s.Close()
	ctx := context.Background()
	ids := make([]string, 0, *count)
	start := time.Now()
	words := []string{"deployment", "revision", "agent", "workshop", "configuration", "design", "client", "server", "backup", "plan", "search", "validation", "protocol", "concurrency", "storage", "certificate"}
	rng := rand.New(rand.NewPCG(17, 42))
	if *measureOnly {
		cursor := ""
		for {
			page, err := s.Page(ctx, api.Filter{Limit: 200, Cursor: cursor, IncludeArchived: true}, false)
			if err != nil {
				panic(err)
			}
			for _, d := range page.Documents {
				if d.Actor != "benchmark" {
					panic("refusing database with non-benchmark documents")
				}
				ids = append(ids, d.ID)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(ids) != *count {
			panic(fmt.Sprintf("expected %d benchmark docs, found %d", *count, len(ids)))
		}
	} else {
		for i := 0; i < *count; i++ {
			size := int(*total / int64(*count))
			if int64(i) < *total%int64(*count) {
				size++
			}
			var b strings.Builder
			b.Grow(size + 100)
			fmt.Fprintf(&b, "# Project %d design plan %d\nUnique identifier project-%05d.example/internal_api\n", i%100, i, i)
			for b.Len() < size {
				fmt.Fprintf(&b, "%s %s %d\n", words[rng.IntN(len(words))], words[rng.IntN(len(words))], rng.IntN(100000))
			}
			content := b.String()[:size]
			r, e := s.Mutate(ctx, "benchmark", fmt.Sprint(i), api.Mutation{Operation: "create", Path: fmt.Sprintf("project%03d/plans/document%05d.md", i%100, i), Content: &content})
			if e != nil {
				panic(e)
			}
			ids = append(ids, r.Document.ID)
			if (i+1)%1000 == 0 {
				fmt.Fprintf(os.Stderr, "created %d/%d documents\n", i+1, *count)
			}
		}
	}
	buildSeconds := time.Since(start).Seconds()
	if *measureOnly {
		buildSeconds = 0
	}
	first := time.Now()
	_, e = s.Page(ctx, api.Filter{Query: "deployment validation", Limit: 50}, true)
	if e != nil {
		panic(e)
	}
	firstSearch := float64(time.Since(first).Microseconds()) / 1000
	reads, searches := make([]float64, 0, *samples), make([]float64, 0, *samples)
	lists := make([]float64, 0, *samples)
	for i := 0; i < *samples; i++ {
		start = time.Now()
		_, e = s.Get(ctx, "", ids[rng.IntN(len(ids))], 0)
		if e != nil {
			panic(e)
		}
		reads = append(reads, float64(time.Since(start).Microseconds())/1000)
		start = time.Now()
		_, e = s.Page(ctx, api.Filter{Query: words[i%len(words)] + " " + words[(i+3)%len(words)], Limit: 50}, true)
		if e != nil {
			panic(e)
		}
		searches = append(searches, float64(time.Since(start).Microseconds())/1000)
		start = time.Now()
		_, e = s.Page(ctx, api.Filter{Limit: 50, Project: fmt.Sprintf("project%03d", i%100)}, false)
		if e != nil {
			panic(e)
		}
		lists = append(lists, float64(time.Since(start).Microseconds())/1000)
	}
	result := map[string]any{"documents": *count, "content_bytes": *total, "samples": *samples, "concurrency": 1, "conditions": "local store; warm OS cache after corpus ingestion; no HTTP/TLS/network; random deterministic technical word corpus; first page only", "build_seconds": buildSeconds, "first_search_ms": firstSearch, "full_get": stats(reads), "default_search": stats(searches), "metadata_list": stats(lists), "measure_only": *measureOnly}
	json.NewEncoder(os.Stdout).Encode(result)
}
