package main

import (
	"fmt"
	"os"

	"alexandria/internal/archive"
)

func main() {
	if err := archive.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
