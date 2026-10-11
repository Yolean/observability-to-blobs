// y-obs answers investigation questions from the observability bucket
// alone (README.md in this directory). Each command is a function taking
// its own arguments; main only dispatches.
package main

import (
	"fmt"
	"os"
)

type command struct {
	name    string
	summary string
	run     func(args []string)
}

// commands in the order an investigation uses them; help prints them so
var commands = []command{
	{"apiserver-at", "serve a state of objects (JSON lines of apiserver_at rows) to kubectl, read-only", cmdApiserverAt},
}

const usage = `y-obs: the observability bucket as the only backend

Usage: y-obs <command> [flags]

Commands:
%s
Run y-obs <command> -h for a command's flags.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		printUsage(os.Stdout)
		return
	}
	for _, c := range commands {
		if c.name == os.Args[1] {
			c.run(os.Args[2:])
			return
		}
	}
	fmt.Fprintf(os.Stderr, "y-obs: unknown command %q; run y-obs help\n", os.Args[1])
	os.Exit(2)
}

func printUsage(w *os.File) {
	list := ""
	for _, c := range commands {
		list += fmt.Sprintf("  %-14s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, usage, list)
}
