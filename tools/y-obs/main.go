// y-obs answers investigation questions from the observability bucket
// alone (README.md in this directory). Each command is a function taking
// its own arguments; main only dispatches and turns errors into the exit
// codes of README.md, "Design rules for LLM callers".
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) error
}

// commands in the order an investigation uses them; help prints them so
var commands = []command{
	{"sql", "a DuckDB query over the views of a window (raw, logs, metrics, podindex, pods, apiserver, hourly_*, daily_*)", cmdSQL},
	{"schema", "the views, or one view's columns and their meanings", cmdSchema},
	{"kubectl", "kubectl against the cluster as it was at a time, read-only", cmdKubectl},
	{"apiserver-at", "serve a state of objects (JSON lines of apiserver_at rows) to kubectl, read-only", func(_ context.Context, args []string) error {
		cmdApiserverAt(args)
		return nil
	}},
}

const usage = `y-obs: the observability bucket as the only backend

Usage: y-obs <command> [flags]

Commands:
%s
Run y-obs <command> -h for a command's flags and examples.

Exit codes: 0 rows printed, 1 nothing matched, 2 usage, 3 credentials,
4 not in the bucket, 5 over --max-objects, 6 the store or the engine failed.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		printUsage(os.Stdout)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, c := range commands {
		if c.name == os.Args[1] {
			os.Exit(exitCode(c.run(ctx, os.Args[2:])))
		}
	}
	fmt.Fprintf(os.Stderr, "y-obs: unknown command %q; run y-obs help\n", os.Args[1])
	os.Exit(exitUsage)
}

// exitCode prints an error's one line and returns its code.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "y-obs: "+ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(os.Stderr, "y-obs: "+err.Error())
	return exitStore
}

func printUsage(w io.Writer) {
	list := ""
	for _, c := range commands {
		list += fmt.Sprintf("  %-14s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, usage, list)
}
