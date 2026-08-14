// Command duckwords analyzes dictionary words in Reddit comment trees.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pointerm/duckwords/internal/cli"
	"github.com/pointerm/duckwords/internal/production"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, production.Execute))
}
