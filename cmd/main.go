package main

import (
	"context"
	"os"

	"github.com/asynkron/goagent/internal/cli"
)

// main bootstraps the Go translation of the GoAgent runtime.
func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
