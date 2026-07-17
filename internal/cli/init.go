package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/feishu"
	"github.com/arcships/open-doc-cli/internal/layout"
)

// feishuAuthScopes is the read-only scope set the mirror needs, verified live on
// the reference app (kept in sync with the scope table in
// plugin/skills/opendoc/references/setup.md). offline_access is required for the
// refresh token
// that keeps unattended syncs alive; everything else is read-only.
const feishuAuthScopes = "offline_access " +
	"docx:document:readonly docs:document.content:read docs:document.media:download " +
	"wiki:space:read wiki:node:retrieve wiki:node:read " +
	"space:document:retrieve drive:drive.metadata:readonly " +
	"bitable:app:readonly board:whiteboard:node:read"

// runInit implements `opendoc init`: it materialises <root>/.internal/config.toml.
// It supports interactive prompting
// (when stdin is a terminal) and full non-interactive operation via flags, so
// agents and CI can drive it. It refuses to overwrite an existing config unless
// --force.
func runInit(env Env, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "mirror root (overrides OPENDOC_ROOT and ~/.opendoc)")
	force := fs.Bool("force", false, "overwrite an existing config")
	noInput := fs.Bool("no-input", false, "never prompt; use flags and defaults only")
	wikiSpaces := fs.String("feishu-wiki-spaces", "", "comma-separated Feishu wiki space IDs")
	driveFolders := fs.String("feishu-drive-folders", "", "comma-separated Feishu drive folder tokens")
	includeMyLibrary := fs.Bool("include-my-library", false, "mirror the personal Feishu library")
	notionTokenEnv := fs.String("notion-token-env", "", "env var holding the Notion integration token (e.g. NOTION_TOKEN); empty disables Notion")
	bitableMaxRows := fs.Int("bitable-inline-max-rows", config.DefaultBitableInlineMaxRows, "inline-render row threshold for bitables")
	trashKeepDays := fs.Int("trash-keep-days", config.DefaultTrashKeepDays, "trash retention window in days")
	notionReconcileEveryRuns := fs.Int("notion-reconcile-every-runs", config.DefaultNotionReconcileEveryRuns, "force a Notion full reconciliation round every N runs (0 = daily-only)")

	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: opendoc init [flags]\n\nGenerate <root>/.internal/config.toml (feishu + sync sections).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	l, err := layout.Resolve(*root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc init: %v\n", err)
		return ExitError
	}

	cfgPath := l.ConfigPath()
	if config.Exists(cfgPath) && !*force {
		fmt.Fprintf(env.Stderr, "opendoc init: config already exists at %s (use --force to overwrite)\n", cfgPath)
		return ExitError
	}

	cfg := config.Default()

	// Track which flags the caller explicitly set, so we only prompt for the
	// rest in interactive mode.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	interactive := !*noInput && isTerminal(env.Stdin)

	if set["feishu-wiki-spaces"] {
		cfg.Feishu.WikiSpaces = splitCSV(*wikiSpaces)
	}
	if set["feishu-drive-folders"] {
		cfg.Feishu.DriveFolders = splitCSV(*driveFolders)
	}
	if set["include-my-library"] {
		cfg.Feishu.IncludeMyLibrary = *includeMyLibrary
	}
	if set["notion-token-env"] {
		cfg.Notion.TokenEnv = strings.TrimSpace(*notionTokenEnv)
	}
	if set["bitable-inline-max-rows"] {
		cfg.Sync.BitableInlineMaxRows = *bitableMaxRows
	}
	if set["trash-keep-days"] {
		cfg.Sync.TrashKeepDays = *trashKeepDays
	}
	if set["notion-reconcile-every-runs"] {
		cfg.Sync.NotionReconcileEveryRuns = *notionReconcileEveryRuns
	}

	if interactive {
		r := bufio.NewReader(env.Stdin)
		fmt.Fprintf(env.Stdout, "opendoc init — configuring %s\n", cfgPath)
		if !set["feishu-wiki-spaces"] {
			cfg.Feishu.WikiSpaces = splitCSV(prompt(env, r, "Feishu wiki space IDs (comma-separated)", strings.Join(cfg.Feishu.WikiSpaces, ",")))
		}
		if !set["feishu-drive-folders"] {
			cfg.Feishu.DriveFolders = splitCSV(prompt(env, r, "Feishu drive folder tokens (comma-separated)", strings.Join(cfg.Feishu.DriveFolders, ",")))
		}
		if !set["include-my-library"] {
			cfg.Feishu.IncludeMyLibrary = promptBool(env, r, "Include personal Feishu library?", cfg.Feishu.IncludeMyLibrary)
		}
		if !set["notion-token-env"] {
			cfg.Notion.TokenEnv = strings.TrimSpace(prompt(env, r, "Notion integration token env var (blank to disable Notion)", cfg.Notion.TokenEnv))
		}
	}

	if err := l.EnsureInternal(); err != nil {
		fmt.Fprintf(env.Stderr, "opendoc init: %v\n", err)
		return ExitError
	}
	if err := config.Write(cfgPath, cfg); err != nil {
		fmt.Fprintf(env.Stderr, "opendoc init: %v\n", err)
		return ExitError
	}

	fmt.Fprintf(env.Stdout, "wrote %s\n", cfgPath)

	// Feishu authorization onboarding. The engine
	// is embedded, so init can guide the whole flow: `config init --new` scans a
	// QR to one-click-create the app, `auth login` grants the user token. Only
	// the interactive path executes anything; non-interactive runs print the
	// commands and leave them to the operator (agents drive them directly).
	feishuConfigured := len(cfg.Feishu.WikiSpaces) > 0 || len(cfg.Feishu.DriveFolders) > 0 || cfg.Feishu.IncludeMyLibrary
	if feishuConfigured {
		setupFeishuAuth(env, l, interactive)
	}
	return ExitOK
}

// engineAuthState classifies the engine's credential state for onboarding.
type engineAuthState int

const (
	authReady   engineAuthState = iota // user token available
	authNoApp                          // no app configured (~/.lark-cli empty)
	authNoToken                        // app configured, user token missing/expired
)

// probeEngineAuth classifies the current Feishu auth state via the engine's
// local, network-free `whoami` (available + tokenStatus fields). An exec
// failure or unparseable output means no app is configured yet: the engine
// errors out before probing when it has no config.
func probeEngineAuth(l layout.Layout) engineAuthState {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bin := config.ResolveEnv(larkCLIEnv, os.Getenv, l.EnvFilePath()).Value
	out, err := feishu.ExecRunner{Bin: bin}.Run(ctx, "whoami")
	var who struct {
		Available bool   `json:"available"`
		AppID     string `json:"appId"`
	}
	if jerr := json.Unmarshal(out, &who); jerr != nil || (err != nil && who.AppID == "") {
		return authNoApp
	}
	if who.Available {
		return authReady
	}
	if who.AppID == "" {
		return authNoApp
	}
	return authNoToken
}

// setupFeishuAuth reports the Feishu auth state and — interactively — walks the
// user through app creation and login via the embedded engine. It never fails
// `opendoc init`: authorization can always be finished later (doctor F2 points back
// here), so problems are reported and left behind as instructions.
func setupFeishuAuth(env Env, l layout.Layout, interactive bool) {
	if !interactive {
		// Never exec anything on the non-interactive path (agents and tests own
		// it): print the full flow and let the operator drive.
		fmt.Fprintf(env.Stdout, "feishu auth: if not yet authorized, run:\n")
		fmt.Fprintf(env.Stdout, "  opendoc lark-engine config init --new   # first time only: one-click app creation\n")
		fmt.Fprintf(env.Stdout, "  opendoc lark-engine auth login --scope %q\n", feishuAuthScopes)
		return
	}

	state := probeEngineAuth(l)
	if state == authReady {
		fmt.Fprintf(env.Stdout, "feishu auth: ready\n")
		return
	}

	steps := [][]string{}
	if state == authNoApp {
		steps = append(steps, []string{"config", "init", "--new"})
	}
	steps = append(steps, []string{"auth", "login", "--scope", feishuAuthScopes})

	r := bufio.NewReader(env.Stdin)
	if !promptBool(env, r, "Set up Feishu authorization now (scan a QR code)?", true) {
		fmt.Fprintf(env.Stdout, "skipped; finish later with `opendoc init` or the commands in references/setup.md\n")
		return
	}
	for _, s := range steps {
		if err := execEngineInteractive(env, l, s...); err != nil {
			fmt.Fprintf(env.Stderr, "opendoc init: feishu auth step `%s` failed: %v\nfinish later per references/setup.md\n", strings.Join(s, " "), err)
			return
		}
	}
	if probeEngineAuth(l) == authReady {
		fmt.Fprintf(env.Stdout, "feishu auth: ready\n")
	} else {
		fmt.Fprintf(env.Stdout, "feishu auth: still not ready — run `opendoc doctor` to diagnose\n")
	}
}

// execEngineInteractive runs one engine subcommand with the process stdio
// attached, so device-flow prompts (QR link, polling) reach the user. It
// honours the OPENDOC_LARK_CLI escape hatch like the sync Runner does.
func execEngineInteractive(env Env, l layout.Layout, args ...string) error {
	bin := config.ResolveEnv(larkCLIEnv, os.Getenv, l.EnvFilePath()).Value
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve opendoc executable: %w", err)
		}
		bin = exe
		args = append([]string{"lark-engine"}, args...)
	}
	cmd := exec.Command(bin, args...)
	// Pass the real terminal fd through when we have one so the child's own
	// TTY-aware prompts work; any other reader still works via a pipe.
	if f, ok := env.Stdin.(*os.File); ok {
		cmd.Stdin = f
	} else {
		cmd.Stdin = env.Stdin
	}
	cmd.Stdout = env.Stdout
	cmd.Stderr = env.Stderr
	cmd.Env = append(os.Environ(), "LARKSUITE_CLI_NO_UPDATE_NOTIFIER=1")
	return cmd.Run()
}

// splitCSV splits a comma-separated list, trimming whitespace and dropping
// empty fields. It returns a non-nil empty slice for empty input so the TOML
// output is a stable [] rather than a missing key.
func splitCSV(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// prompt asks for a value, showing the current default, and returns the entered
// value or the default on empty input.
func prompt(env Env, r *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Fprintf(env.Stdout, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(env.Stdout, "%s: ", label)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptBool asks a yes/no question with a default.
func promptBool(env Env, r *bufio.Reader, label string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	fmt.Fprintf(env.Stdout, "%s [%s]: ", label, d)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	switch line {
	case "":
		return def
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// isTerminal reports whether r is an interactive terminal. It only recognises
// *os.File backed by a character device, so tests using buffers are treated as
// non-interactive.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
