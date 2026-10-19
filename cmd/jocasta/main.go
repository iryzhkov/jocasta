package main

import (
	"context"
	"github.com/iryzhkov/jocasta/internal/buildinfo"
	"github.com/iryzhkov/jocasta/internal/cli"
	"os"
)

func main() {
	os.Exit(cli.RunSpooled(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, buildinfo.Version))
}
