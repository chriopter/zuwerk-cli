package main

import (
	"fmt"
	"io"
	"os"
)

const version = "0.0.1"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintf(stdout, "zuwerk %s\n", version)
		return 0
	}

	fmt.Fprintln(stderr, "usage: zuwerk version")
	return 2
}
