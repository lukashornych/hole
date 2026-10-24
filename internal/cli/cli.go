// Package cli parses Hole's command line and dispatches to the commands.
//
// The parser is hand-rolled on purpose: flags and positionals interleave freely before
// `--`, and everything after `--` goes to the agent verbatim. Frameworks with interspersed
// argument handling make that contract harder to hold exactly, not easier.
package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/sandbox"
	"github.com/lukashornych/hole/v2/internal/update"
	"github.com/lukashornych/hole/v2/internal/version"
)

// runLogRetention is how long per-run debug logs are kept; cleanup rides sandbox startup.
const runLogRetention = 7 * 24 * time.Hour

const helpText = `Usage: hole {command} {agent}[:{profile}] {path} [options]
       hole settings {create|open|get} {project {path}|global} [options]

Commands:
  start     Create a sandbox, attach to the agent CLI, and destroy on exit
  list      Show running sandboxes
  destroy   Remove Docker resources for a project, or all resources if no path given
  update    Update hole to the latest release
  uninstall Uninstall hole and optionally remove Docker resources
  settings  Create, open or locate the project or global settings file
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
      --update-agents         Update the enabled agent CLIs inside the existing image
                                  before starting, without a full rebuild
      --library PATH[:MOUNT][:rw]  Mount an extra directory (repeatable). Mounted at
                                  its own host path unless MOUNT is given, and
                                  read-only unless :rw is given
      --with-docker           Enable Docker-in-Docker sidecar for the sandbox
      --trust-project         Accept the host access this project's settings
                                  asks for without being asked, and remember it
      --instance-id ID        Use this instance ID (6 characters a-z, 0-9) instead
                                  of a random one; start only
      --json                  Print running sandboxes as JSON; list only
      --no-gc                 Do not clean up leftovers of dead sandboxes while
                                  listing; list only
      --overwrite             Replace an existing settings file with the
                                  template; settings create only
      --append                Add the template's fields to an existing
                                  settings file; settings create only
  --                          Separator for agent-specific arguments;
                                  everything after -- is passed to the agent CLI

Configure file exclusions, inclusions, libraries, allowed domains, dependencies,
environment variables, container settings, agent arguments and hooks via
.hole/settings.json (per-project, or the file named by $HOLE_PROJECT_SETTINGS_FILE)
or ~/.hole/settings.json (global, or the file named by $HOLE_SETTINGS_FILE).
Named overlays go under the "profiles" key and are selected with
{agent}:{profile}.

Examples:
  hole start claude .
  hole start claude /path/to/project
  hole start claude:research .            # start with the "research" profile applied
  hole start claude . --library ~/other-repo --library ~/lib:/libs/lib:rw
  hole start claude . --dump-network-access
  hole start claude . -- -p "explain this function"
  hole start claude . --rebuild -- --output-format stream-json
  hole start claude . --update-agents     # newest agent CLIs, setup hooks not re-run
  hole start claude . --instance-id r8tq2c
  hole list                               # show running sandboxes
  hole list --json                        # running sandboxes as JSON
  hole list --json r8tq2c                 # one sandbox as JSON, [] if not running
  hole list --json --no-gc r8tq2c         # same, without cleaning up (for polling)
  hole destroy                            # destroy ALL Hole Docker resources
  hole destroy .                          # destroy resources for current project
  hole destroy /path/to/project           # destroy resources for specific project
  hole settings create project .          # write .hole/settings.json with $schema
  hole settings create global --append    # add $schema to ~/.hole/settings.json
  hole settings open project .            # edit and validate .hole/settings.json
  hole settings get global                # print the global settings file path

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
	TrustProject      bool
	UpdateAgents      bool
	// Libraries are the raw --library values, in the order they were given.
	Libraries []string
	// InstanceID is the --instance-id value, already format-validated; start only.
	InstanceID string
	// JSON is --json; list only.
	JSON bool
	// NoGC is --no-gc; list only.
	NoGC bool
	// Overwrite is --overwrite; settings create only.
	Overwrite bool
	// Append is --append; settings create only.
	Append bool
}

// Parse turns raw arguments into an invocation.
func Parse(args []string) (*Invocation, error) {
	inv := &Invocation{}
	parsingHoleArgs := true
	expectLibrary := false
	expectInstanceID := false
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
		if expectInstanceID {
			if err := hostenv.ValidateInstanceID(arg); err != nil {
				return nil, err
			}
			inv.InstanceID = arg
			expectInstanceID = false
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
		case "--trust-project":
			inv.TrustProject = true
		case "--update-agents":
			inv.UpdateAgents = true
		case "--library":
			// The value is the next argument; the `--library=x` form is handled below.
			expectLibrary = true
		case "--instance-id":
			expectInstanceID = true
		case "--json":
			inv.JSON = true
		case "--no-gc":
			inv.NoGC = true
		case "--overwrite":
			inv.Overwrite = true
		case "--append":
			inv.Append = true
		case "--":
			parsingHoleArgs = false
		default:
			if value, ok := strings.CutPrefix(arg, "--library="); ok {
				inv.Libraries = append(inv.Libraries, value)
				continue
			}
			if value, ok := strings.CutPrefix(arg, "--instance-id="); ok {
				if err := hostenv.ValidateInstanceID(value); err != nil {
					return nil, err
				}
				inv.InstanceID = value
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
	if expectInstanceID {
		return nil, fmt.Errorf("--instance-id needs a value: 6 characters a-z, 0-9")
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
	if inv.Command != "start" && inv.InstanceID != "" {
		logging.Error("--instance-id can only be used with the start command")
		return 1
	}
	if inv.Command != "list" && inv.JSON {
		logging.Error("--json can only be used with the list command")
		return 1
	}
	if inv.Command != "list" && inv.NoGC {
		logging.Error("--no-gc can only be used with the list command")
		return 1
	}
	if inv.Command != "settings" && (inv.Overwrite || inv.Append) {
		logging.Error("--overwrite and --append can only be used with the settings command")
		return 1
	}

	switch inv.Command {
	case "", "help":
		fmt.Print(helpText)
		return 0
	case "version":
		fmt.Printf("hole %s\n", version.Display())
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
	case "settings":
		return runSettings(inv, settingsIO{
			stdin:    bufio.NewReader(os.Stdin),
			stdout:   os.Stdout,
			stderr:   os.Stderr,
			terminal: isTerminal(os.Stdin) && isTerminal(os.Stdout),
		})
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
		logging.Info("Valid commands: start list destroy settings version update uninstall help")
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
		TrustProject:      inv.TrustProject,
		UpdateAgents:      inv.UpdateAgents,
		Libraries:         inv.Libraries,
		AgentArgs:         inv.AgentArgs,
		InstanceID:        inv.InstanceID,
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
	if len(inv.Positional) > 1 {
		logging.Error("unexpected argument '%s'; usage: hole list [--json] [instance-id]", inv.Positional[1])
		return 1
	}
	var instanceID string
	if len(inv.Positional) == 1 {
		instanceID = inv.Positional[0]
		if err := hostenv.ValidateInstanceID(instanceID); err != nil {
			logging.Error("%v", err)
			return 1
		}
	}
	if err := sandbox.List(os.Stdout, sandbox.ListOptions{JSON: inv.JSON, NoGC: inv.NoGC, InstanceID: instanceID}); err != nil {
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
	// An unusable container runtime must not abort the uninstall: it would leave the binary and
	// the user directory installed, with the runtime error as the only thing the user sees.
	containerEngine, err := engine.Detect()
	if err != nil {
		logging.Warn("could not detect the container runtime: %v", err)
	}
	removeSettings := update.ConfirmRemoveSettings(os.Stdin, os.Stderr, host.HoleDir(), isTerminal(os.Stdin))
	update.Uninstall(host, containerEngine, update.UninstallOptions{RemoveSettings: removeSettings})
	return 0
}

// isTerminal reports whether a file is attached to a terminal, which decides whether
// uninstall and settings open may ask a question, and whether settings open may fall back to vi.
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
