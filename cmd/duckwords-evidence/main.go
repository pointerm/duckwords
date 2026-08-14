// Command duckwords-evidence validates captured DuckWords output and atomically
// creates the submission evidence directory. It is an offline release tool and does
// not access Reddit or accept credentials.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/pointerm/duckwords/internal/evidence"
)

const usage = `Usage:
  duckwords-evidence --result PATH --log PATH --output-dir DIR \
    --exit-code 0|3 --binary PATH --policy-verified-at YYYY-MM-DD \
    --approval-reference SAFE_ID
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	if !validArguments(args) {
		_, _ = fmt.Fprintf(stderr, "duckwords-evidence: invalid arguments\n%s", usage)
		return 2
	}
	flags := flag.NewFlagSet("duckwords-evidence", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg evidence.Config
	flags.StringVar(&cfg.ResultPath, "result", "", "captured canonical stdout JSON")
	flags.StringVar(&cfg.LogPath, "log", "", "captured JSON operational log")
	flags.StringVar(&cfg.OutputDir, "output-dir", "", "new evidence directory")
	flags.IntVar(&cfg.ExitCode, "exit-code", -1, "DuckWords exit code (0 or 3)")
	flags.StringVar(&cfg.BinaryPath, "binary", "", "executed release binary")
	flags.StringVar(&cfg.PolicyVerifiedAt, "policy-verified-at", "", "Reddit policy verification date")
	flags.StringVar(&cfg.ApprovalReference, "approval-reference", "", "non-secret approval attestation reference")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "duckwords-evidence: invalid arguments\n%s", usage)
		return 2
	}
	if _, err := evidence.Finalize(ctx, cfg); err != nil {
		_, _ = fmt.Fprintln(stderr, "duckwords-evidence: evidence validation or publication failed")
		return 1
	}
	return 0
}

func validArguments(args []string) bool {
	if len(args) != 14 {
		return false
	}
	allowed := map[string]struct{}{
		"--result": {}, "--log": {}, "--output-dir": {}, "--exit-code": {}, "--binary": {},
		"--policy-verified-at": {}, "--approval-reference": {},
	}
	seen := make(map[string]struct{}, len(allowed))
	for index := 0; index < len(args); index += 2 {
		name, value := args[index], args[index+1]
		if _, ok := allowed[name]; !ok || value == "" {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return len(seen) == len(allowed)
}
