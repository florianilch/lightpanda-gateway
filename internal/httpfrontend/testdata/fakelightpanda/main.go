package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		run()
	default:
		os.Exit(2)
	}
}

func run() {
	if len(os.Args) < 3 {
		os.Exit(2)
	}
	script, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := fmt.Fprintf(os.Stdout, "fake-lightpanda:%s", script); err != nil {
		os.Exit(2)
	}
}
