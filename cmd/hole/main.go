// Command hole creates and manages Docker-based sandboxes for AI agents.
package main

import (
	"os"

	"github.com/lukashornych/hole/v2/internal/cli"
	"github.com/lukashornych/hole/v2/internal/logging"
)

func main() {
	closeLog, err := logging.Setup(logging.Options{})
	if err != nil {
		panic(err)
	}
	defer closeLog()

	os.Exit(cli.Run(os.Args[1:]))
}
