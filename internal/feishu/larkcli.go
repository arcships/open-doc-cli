// Package feishu is the Feishu (Lark) platform adapter. Enumeration
// and metadata go through the `api` passthrough of the embedded lark engine;
// document bodies are fetched via `docs +fetch` (markdown for the body, XML for
// asset tokens and doc-link references). Credentials are fully managed by the
// engine (config + keychain under ~/.lark-cli); opendoc never handles Feishu secrets.
//
// The engine is the official Feishu CLI (github.com/larksuite/cli) compiled
// into the opendoc binary itself and reached by re-exec'ing the current executable
// with the hidden `lark-engine` subcommand (see docs/dev/architecture.md), so opendoc
// ships with no external lark-cli/Node dependency.
//
// All engine invocations funnel through the Runner interface so tests can
// substitute canned responses with no network or binary dependency.
package feishu

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Runner executes a lark engine invocation and returns its stdout.
// Implementations must respect ctx cancellation. The error, when non-nil,
// should carry stderr context so callers can classify failures (e.g. rate
// limiting).
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
	// RunInDir is Run with the process working directory set to dir. It exists
	// for `docs +media-download`, whose --output must be a relative path within
	// the current directory; the asset downloader sets dir to the destination
	// directory and passes the base name as --output.
	RunInDir(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// ExecRunner is the production Runner. With Bin empty (the default) it runs
// the embedded engine: the current opendoc executable re-exec'd with the hidden
// `lark-engine` prefix. Bin set to a path or name (the OPENDOC_LARK_CLI escape
// hatch) runs that external lark-cli binary instead, unprefixed — useful to
// A/B against another engine version when debugging.
type ExecRunner struct {
	// Bin is an external lark-cli binary path or name; empty means embedded.
	Bin string
}

// Run executes the engine with args and returns stdout. On a non-zero exit it
// returns an error that includes stderr, so backoff logic can inspect the
// message for rate-limit signals.
func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.RunInDir(ctx, "", args...)
}

// RunInDir executes the engine with args and the working directory set to dir
// (unchanged when dir is empty), returning stdout.
func (r ExecRunner) RunInDir(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve opendoc executable for embedded lark engine: %w", err)
		}
		bin = exe
		args = append([]string{"lark-engine"}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// The engine version is locked by opendoc's go.mod; keep the upstream update
	// notifier dormant in every invocation (an embedded engine must never try to
	// update — that would mean replacing the opendoc binary itself).
	cmd.Env = append(os.Environ(), "LARKSUITE_CLI_NO_UPDATE_NOTIFIER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("lark engine %v: %w: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
