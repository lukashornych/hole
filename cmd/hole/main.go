// Command hole creates and manages Docker-based sandboxes for AI agents.
package main

import (
	"os"

	"github.com/lukashornych/hole/internal/cli"
	"github.com/lukashornych/hole/internal/logging"
)

func main() {
	closeLog, err := logging.Setup(logging.Options{})
	if err != nil {
		panic(err)
	}
	defer closeLog()

	os.Exit(cli.Run(os.Args[1:]))
}
