// Command tuzy is the Tuzy tunnel CLI.
package main

import (
	"fmt"
	"os"

	"github.com/nhtera/tuzy/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
