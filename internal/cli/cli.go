// Package cli implements the `opendoc` subcommand dispatch. It follows the
// "agent-first" machine-friendly principles: structured output,
// deterministic exit codes, non-interactive by default — the sole exception is
// `opendoc init`, which may prompt but must also run fully from flags.
//
// Dispatch is a small stdlib flag-based switch rather than a third-party
// command framework, to keep dependencies minimal.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/layout"
)

// Exit codes are deterministic so scripts and agents can branch on them.
const (
	// ExitOK is a successful run.
	ExitOK = 0
	// ExitError is a runtime failure (I/O, config, manifest, ...).
	ExitError = 1
	// ExitUsage is a bad invocation (unknown command, bad flags).
	ExitUsage = 2
	// ExitNotInitialized signals that no config.toml exists at the resolved root:
	// the mirror has never been initialized. Every command except `opendoc init`
	// returns it (and points at references/setup.md on stderr) so an agent or a
	// bare-terminal user is routed into onboarding. `opendoc doctor` is a special case:
	// it still prints its full report first, then exits with this code.
	ExitNotInitialized = 3
)

// requireInitialized returns ExitNotInitialized — after pointing at
// references/setup.md on stderr — when no config.toml exists at the resolved
// root, or -1 when the mirror is initialized. `opendoc init` is exempt; `opendoc doctor`
// handles the uninitialized case itself. The stderr pointer is itself the
// onboarding trigger: an agent that sees it loads references/setup.md.
func requireInitialized(env Env, l layout.Layout, cmd string) int {
	if config.Exists(l.ConfigPath()) {
		return -1
	}
	fmt.Fprintf(env.Stderr, "opendoc %s: no config at `%s`; run first-time setup per references/setup.md\n", cmd, l.ConfigPath())
	return ExitNotInitialized
}

// Env carries the process I/O and environment so commands are testable.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Args are the arguments after the program name.
	Args []string
}

// DefaultEnv builds an Env from the real process.
func DefaultEnv() Env {
	return Env{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Args:   os.Args[1:],
	}
}

const usage = `opendoc — mirror Notion/Feishu docs to a local Markdown tree

Usage:
  opendoc <command> [flags]

Commands:
  init      Generate <root>/.internal/config.toml
  sync      Run one pass of the sync pipeline
  status    Print a manifest overview (documents, last sync, degradations)
  doctor    Run credential/tooling self-checks
  resolve   Look up a document by id, URL, or local path
  schedule  Install/inspect the unattended twice-daily sync (launchd, macOS)

Common flags:
  --root <path>   Mirror root (overrides OPENDOC_ROOT env and the ~/.opendoc default)

Run "opendoc <command> -h" for command-specific flags.
`

// Run dispatches a single invocation and returns the process exit code.
func Run(env Env) int {
	if len(env.Args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	cmd, rest := env.Args[0], env.Args[1:]
	switch cmd {
	case "init":
		return runInit(env, rest)
	case "sync":
		return runSync(env, rest)
	case "status":
		return runStatus(env, rest)
	case "doctor":
		return runDoctor(env, rest)
	case "resolve":
		return runResolve(env, rest)
	case "schedule":
		return runSchedule(env, rest)
	case "unschedule":
		// Thin alias for `opendoc schedule --remove`, honouring --root/--json passthrough.
		return runSchedule(env, append([]string{"--remove"}, rest...))
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "opendoc: unknown command %q\n\n%s", cmd, usage)
		return ExitUsage
	}
}
