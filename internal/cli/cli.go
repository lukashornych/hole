// Package cli parses Hole's command line and dispatches to the commands.
//
// The parser is hand-rolled on purpose: flags and positionals interleave freely before
// `--`, and everything after `--` goes to the agent verbatim. Frameworks with interspersed
// argument handling make that contract harder to hold exactly, not easier.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/logging"
	"github.com/lukashornych/hole/internal/sandbox"
	"github.com/lukashornych/hole/internal/update"
	"github.com/lukashornych/hole/internal/version"
)

// runLogRetention is how long per-run debug logs are kept; cleanup rides sandbox startup.
const runLogRetention = 7 * 24 * time.Hour

const helpText = `Usage: hole {command} {agent}[:{profile}] {path} [options]

Commands:
  start     Create a sandbox, attach to the agent CLI, and destroy on exit
  list      Show running sandboxes
  destroy   Remove Docker resources for a project, or all resources if no path given
  update    Update hole to the latest release
  uninstall Uninstall hole and optionally remove Docker resources
  help      Show this help message
  version   Print the installed hole version

Options:
  -d, --debug                 Open a bash shell instead of the agent CLI for
                                  inspecting the sandbox environment
  -n, --dump-network-access   After the agent exits, write the domains the sandbox
                                  resolved (and those it was refused) to
                                  .hole/logs/network-access-{agent}-{id}.log
  -r, --rebuild               Force rebuild of Docker images before starting
  -u, --unrestricted-network  Disable egress filtering; allow all network access
      --library PATH[:MOUNT][:rw]  Mount an extra directory (repeatable). Defaults to
                                  /libs/{basename}, read-only unless :rw is given
      --with-docker           Enable Docker-in-Docker sidecar for the sandbox
  --                          Separator for agent-specific arguments;
                                  everything after -- is passed to the agent CLI

Configure file exclusions, inclusions, libraries, allowed domains, dependencies,
environment variables, container settings, agent arguments and hooks via
.hole/settings.json (per-project) or ~/.hole/settings.json (global). Named overlays go
under the "profiles" key and are selected with {agent}:{profile}.

Examples:
  hole start claude .
  hole start claude /path/to/project
  hole start claude:research .            # start with the "research" profile applied
  hole start claude . --library ~/other-repo --library ~/lib:/libs/lib:rw
  hole start claude . --dump-network-access
  hole start claude . -- -p "explain this function"
  hole start claude . --rebuild -- --output-format stream-json
  hole list                               # show running sandboxes
  hole destroy                            # destroy ALL Hole Docker resources
  hole destroy .                          # destroy resources for current project
  hole destroy /path/to/project           # destroy resources for specific project

The sandbox is destroyed when you exit the agent CLI.
`

// Invocation is a parsed command line.
type Invocation struct {
	Command    string
	Positional []string
	AgentArgs  []string

	Debug             bool
	DumpNetworkAccess bool
	Rebuild           bool
	Unrestricted      bool
	WithDocker        bool
	// Libraries are the raw --library values, in the order they were given.
	Libraries []string
}

// Parse turns raw arguments into an invocation.
func Parse(args []string) (*Invocation, error) {
	inv := &Invocation{}
	parsingHoleArgs := true
	expectLibrary := false
	for _, arg := range args {
		if !parsingHoleArgs {
			inv.AgentArgs = append(inv.AgentArgs, arg)
			continue
		}
		if expectLibrary {
			inv.Libraries = append(inv.Libraries, arg)
			expectLibrary = false
			continue
		}
		switch arg {
		case "-d", "--debug":
			inv.Debug = true
		case "-n", "--dump-network-access":
			inv.DumpNetworkAccess = true
		case "-r", "--rebuild":
			inv.Rebuild = true
		case "-u", "--unrestricted-network":
			inv.Unrestricted = true
		case "--with-docker":
			inv.WithDocker = true
		case "--library":
			// The value is the next argument; the `--library=x` form is handled below.
			expectLibrary = true
		case "--":
			parsingHoleArgs = false
		default:
			if value, ok := strings.CutPrefix(arg, "--library="); ok {
				inv.Libraries = append(inv.Libraries, value)
				continue
			}
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return nil, fmt.Errorf("unknown option '%s'; run 'hole help' for usage", arg)
			}
			inv.Positional = append(inv.Positional, arg)
		}
	}

	if expectLibrary {
		return nil, fmt.Errorf("--library needs a value: PATH[:MOUNT][:rw]")
	}

	if len(inv.Positional) > 0 {
		inv.Command = inv.Positional[0]
		inv.Positional = inv.Positional[1:]
	}
	return inv, nil
}

// Run executes a parsed command line and returns the process exit code.
func Run(args []string) int {
	inv, err := Parse(args)
	if err != nil {
		logging.Error("%v", err)
		return 1
	}

	// A profile selects a settings overlay for one sandbox, so it is meaningless anywhere
	// except `start` — accepting it silently elsewhere would hide a typo.
	if inv.Command != "start" && len(inv.Positional) > 0 && strings.Contains(inv.Positional[0], ":") {
		logging.Error("profiles can only be used with the start command")
		return 1
	}

	switch inv.Command {
	case "", "help":
		fmt.Print(helpText)
		return 0
	case "version":
		fmt.Printf("hole %s\n", version.Version)
		update.CheckForUpdate()
		return 0
	case "update":
		return runUpdate(inv)
	case "uninstall":
		return runUninstall(inv)
	case "start":
		return runStart(inv)
	case "list":
		return runList(inv)
	case sandbox.WatchdogCommand:
		// Hidden: the detached teardown supervisor the CLI spawns for every sandbox.
		if len(inv.Positional) != 1 {
			return 1
		}
		return sandbox.RunWatchdog(inv.Positional[0])
	case "destroy":
		return runDestroy(inv)
	default:
		logging.Error("invalid command '%s'", inv.Command)
		logging.Info("Valid commands: start list destroy version update uninstall help")
		return 1
	}
}

func runStart(inv *Invocation) int {
	if len(inv.Positional) == 0 {
		logging.Error("no agent given")
		logging.Info("Usage: hole start {agent} {path} [options]")
		return 1
	}
	// A missing project path used to silently mean the current directory. Starting a
	// sandbox is not a place for an implicit target, so it is now an error.
	if len(inv.Positional) == 1 {
		logging.Error("no project path given")
		logging.Info("Usage: hole start {agent} {path} [options] — use '.' for the current directory")
		return 1
	}
	if len(inv.Positional) > 2 {
		logging.Error("unexpected argument '%s'; agent arguments go after '--'", inv.Positional[2])
		return 1
	}
	if inv.Debug && len(inv.AgentArgs) > 0 {
		logging.Error("--debug and agent arguments (after --) cannot be used together")
		return 1
	}

	// The agent positional splits on the first colon, so an agent name can never contain one.
	agentName, profile, _ := strings.Cut(inv.Positional[0], ":")
	if profile != "" || strings.Contains(inv.Positional[0], ":") {
		if err := config.ValidateProfileName(profile); err != nil {
			logging.Error("%v", err)
			return 1
		}
	}

	projectDir, err := hostenv.ResolveProjectDir(inv.Positional[1])
	if err != nil {
		logging.Error("%v", err)
		return 1
	}

	host := hostenv.DetectHost()
	logFile := runLogFile(host, agentName)
	closeLog, err := logging.Setup(logging.Options{
		Debug:   inv.Debug,
		LogFile: logFile,
	})
	if err != nil {
		logging.Warn("could not open run log file: %v", err)
	}
	defer closeLog()
	logging.LogFileGC(host.LogDir(), runLogRetention)

	update.CheckForUpdate()

	exitCode, err := sandbox.Start(sandbox.Options{
		Agent:             agentName,
		Profile:           profile,
		LogFile:           logFile,
		ProjectDir:        projectDir,
		Debug:             inv.Debug,
		Rebuild:           inv.Rebuild,
		Unrestricted:      inv.Unrestricted,
		DumpNetworkAccess: inv.DumpNetworkAccess,
		WithDocker:        inv.WithDocker,
		Libraries:         inv.Libraries,
		AgentArgs:         inv.AgentArgs,
	})
	if err != nil {
		logging.Error("%v", err)
		if exitCode == 0 {
			return 1
		}
	}
	return exitCode
}

func runList(inv *Invocation) int {
	if len(inv.Positional) > 0 {
		logging.Error("unexpected argument '%s'; usage: hole list", inv.Positional[0])
		return 1
	}
	if err := sandbox.List(os.Stdout); err != nil {
		logging.Error("%v", err)
		return 1
	}
	return 0
}

func runDestroy(inv *Invocation) int {
	// `hole destroy <path>` used to put the path in the agent slot and destroy the current
	// directory's project instead. Destroy now has its own positional shape.
	if len(inv.Positional) == 0 {
		if err := sandbox.DestroyAll(); err != nil {
			logging.Error("%v", err)
			return 1
		}
		return 0
	}
	if len(inv.Positional) > 1 {
		logging.Error("unexpected argument '%s'; usage: hole destroy [path]", inv.Positional[1])
		return 1
	}
	projectDir, err := hostenv.ResolveProjectDir(inv.Positional[0])
	if err != nil {
		logging.Error("%v", err)
		return 1
	}
	if err := sandbox.Destroy(projectDir); err != nil {
		logging.Error("%v", err)
		return 1
	}
	return 0
}

func runUpdate(inv *Invocation) int {
	if len(inv.Positional) > 0 {
		logging.Error("unexpected argument '%s'; usage: hole update", inv.Positional[0])
		return 1
	}
	if err := update.SelfUpdate(); err != nil {
		logging.Error("%v", err)
		return 1
	}
	return 0
}

func runUninstall(inv *Invocation) int {
	if len(inv.Positional) > 0 {
		logging.Error("unexpected argument '%s'; usage: hole uninstall", inv.Positional[0])
		return 1
	}
	host := hostenv.DetectHost()
	containerEngine, err := engine.Detect()
	if err != nil {
		logging.Error("%v", err)
		return 1
	}
	removeSettings := update.ConfirmRemoveSettings(os.Stdin, os.Stderr, host.HoleDir(), isTerminal(os.Stdin))
	update.Uninstall(host, containerEngine, update.UninstallOptions{RemoveSettings: removeSettings})
	return 0
}

// isTerminal reports whether a file is attached to a terminal, which decides whether
// uninstall may ask a question.
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runLogFile is the per-run debug log path. It is derived from the agent name and the
// process ID because the instance ID is only generated once startup begins.
func runLogFile(host hostenv.Host, agent string) string {
	name := fmt.Sprintf("run-%s-%s-%d.log", time.Now().Format("20060102-150405"), agent, os.Getpid())
	return filepath.Join(host.LogDir(), name)
}
