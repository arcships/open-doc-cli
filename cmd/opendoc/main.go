// Command opendoc mirrors Notion/Feishu docs into a local Markdown tree. It is the
// deterministic sync engine delivered inside the opendoc Agent Skill.
//
// This binary is a thin shell around internal/cli, which owns subcommand
// dispatch and exit codes.
//
// It is also a multi-call binary: the hidden `opendoc lark-engine ...` entry runs
// the embedded Feishu CLI engine (github.com/larksuite/cli, pinned in go.mod)
// in-process, so opendoc ships with no external lark-cli/Node dependency
// (see docs/dev/architecture.md). internal/feishu re-execs the current
// executable with this prefix; agents can also invoke it directly for
// drill-downs (e.g. `opendoc lark-engine api GET ...`).
package main

import (
	"os"

	larkcmd "github.com/larksuite/cli/cmd"
	_ "github.com/larksuite/cli/extension/credential/env" // env-var credential provider (unattended runs)

	"github.com/arcships/open-doc-cli/internal/cli"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "lark-engine" {
		// The embedded engine is version-locked by go.mod; keep the upstream
		// update notifier dormant even if a future build gains a real module
		// version (an embedded engine must never try to update itself).
		if os.Getenv("LARKSUITE_CLI_NO_UPDATE_NOTIFIER") == "" {
			os.Setenv("LARKSUITE_CLI_NO_UPDATE_NOTIFIER", "1")
		}
		os.Args = append([]string{"lark-cli"}, os.Args[2:]...)
		os.Exit(larkcmd.Execute())
	}
	os.Exit(cli.Run(cli.DefaultEnv()))
}
