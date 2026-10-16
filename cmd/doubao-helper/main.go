package main

import (
	"doubao-cpa-plugin/internal/doubao"
	"fmt"
	"os"
)

func main() {
	if err := doubao.RunCLI(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
