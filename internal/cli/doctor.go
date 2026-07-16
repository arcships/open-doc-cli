package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/engine"
	"github.com/arcships/open-doc-cli/internal/feishu"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
	"github.com/arcships/open-doc-cli/internal/notion"
)

// Probe status literals for `opendoc doctor`. warn is advisory and does NOT affect the
// exit code; only fail does.
const (
	checkPass = "pass"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

// feishuAssetDailyQuota is Feishu's asset download ceiling per day (10k).
// doctor estimates the day's usage against it.
const feishuAssetDailyQuota = 10000

// quotaWarnFraction is the low-water threshold for G2: doctor warns (never fails)
// when the estimated remaining asset quota drops below this fraction of the daily
// ceiling, or is exhausted. 20% is a sensible margin for an image-heavy first
// mirror whose remaining budget may not cover the run (resumes next day).
const quotaWarnFraction = 5 // remaining < quota/5 ⇒ below 20%

// minLarkCLIVersion is the locked lower bound for an *external* lark-cli binary
// (F1-VERSION), which only applies under the OPENDOC_LARK_CLI escape hatch. The
// adapter was built and verified against lark-cli v1.0.69 (P0 live-tested), so that is
// the floor. A parseable version below it fails F1; an *unparseable* version
// does not (the F4 drift probe is the alarm for output-shape changes). The
// default embedded engine needs no floor: its version is pinned by go.mod.
const minLarkCLIVersion = "1.0.69"

// larkCLIEnv overrides the embedded lark engine with an external lark-cli
// binary (matching how larkcli.go abstracts exec via ExecRunner.Bin); empty —
// the default — runs the engine embedded in the opendoc binary itself.
const larkCLIEnv = "OPENDOC_LARK_CLI"

// engineModulePath is the embedded Feishu engine's Go module path, used to
// resolve its pinned version from the running binary's build info.
const engineModulePath = "github.com/larksuite/cli"

// embeddedEngineVersion reports the embedded engine's pinned module version
// from the binary's build info, or "" when unavailable (e.g. in a test binary
// that does not link the engine).
func embeddedEngineVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == engineModulePath {
				return dep.Version
			}
		}
	}
	return ""
}

// checkResult is one probe outcome. ID is the probe id (G0, F2, N3, ...); Status
// is pass|warn|fail|skip; Code is the structured failure code (e.g. F2-NOAUTH,
// N3-EMPTY) and is empty for pass/warn/skip; Detail is a one-line
// human explanation.
type checkResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail"`
}

// doctorReport is the structured `opendoc doctor --json` payload. Initialized mirrors
// whether config.toml exists (the G0 state oracle); OK is true iff no
// probe failed (warn/skip/pass do not fail the run).
type doctorReport struct {
	Root        string        `json:"root"`
	Initialized bool          `json:"initialized"`
	OK          bool          `json:"ok"`
	Checks      []checkResult `json:"checks"`
}

// doctor holds the probes' injectable collaborators so every probe is
// unit-testable with fakes — a fake lark-cli Runner (canned envelopes, including
// auth-expired and permission-denied shapes), fake Notion verify/search closures,
// a stubbed writability check, and a fixed clock — with no network, no real
// binary, and no credentials.
type doctor struct {
	cfg config.Config
	// initialized reports whether config.toml exists (G0). When false the platform
	// probes skip and the command exits ExitNotInitialized.
	initialized bool
	// root is the resolved mirror root (G1 writability target).
	root string
	// db is the opened manifest, or nil when none exists yet.
	db *manifest.DB
	// getenv resolves environment variables (the Notion token; the OPENDOC_LARK_CLI override).
	getenv func(string) string
	// lookPath reports where an external lark-cli binary resolves on PATH (F1
	// presence, escape-hatch mode only).
	lookPath func(string) (string, error)
	// runner runs lark engine invocations (F1 version, F2/F4 user_info, F3/F5 probes).
	runner feishu.Runner
	// larkBin is the external binary name/path probed; empty — the default —
	// means the embedded engine (F1 then reports the pinned module version).
	larkBin string
	// engineVersion resolves the embedded engine's pinned version (production:
	// embeddedEngineVersion via build info).
	engineVersion func() string
	// verifyNotion validates a Notion token (production: notion.Client.Me) — N2.
	verifyNotion func(ctx context.Context, token string) error
	// searchNotion returns the first-page visible page/database count for a token
	// (production: notion.Client.VisibleCount) — N3.
	searchNotion func(ctx context.Context, token string) (int, error)
	// checkWritable reports whether the mirror root is writable (nil ⇒ writable) — G1.
	checkWritable func(root string) error
	// feishuSampleDoc returns a mirrored Feishu docx id to piggyback the F3 docs
	// scope probe on, or ok=false when the manifest holds none (probe then skips).
	feishuSampleDoc func() (string, bool)
	// envFilePath is <root>/.internal/env — the fallback file N1/N2/N3 read a token
	// env var from when it is absent from the process environment (fix 4). Empty
	// disables the file channel.
	envFilePath string
	// forceFeishu / forceNotion force a platform's probe lines to run even when the
	// config does not enable it (`opendoc doctor --platform ...`, fix 5). When forced,
	// feishuConfigured()/notionEnabled() report true.
	forceFeishu bool
	forceNotion bool
	// now is the clock (for the day boundary in the quota estimate).
	now func() time.Time

	// userInfo memoizes the single lark-cli user_info invocation shared by F2 and
	// F4 (reuse the one call, do not probe twice).
	userInfoDone bool
	userInfoOut  []byte
	userInfoErr  error
}

// runDoctor implements `opendoc doctor`: the probe layer with a
// deterministic exit code. It always runs the full report — even without config —
// so NOT_INITIALIZED (G0) is a normal diagnostic, not a crash. Exit: uninitialized
// ⇒ ExitNotInitialized (after printing the report); initialized ⇒ 0 when all
// pass/warn/skip, 1 on any fail.
func runDoctor(env Env, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "mirror root (overrides OPENDOC_ROOT and ~/.opendoc)")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	var platforms platformList
	fs.Var(&platforms, "platform", "force these platforms' probes to run even when config does not enable them (feishu,notion; repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: opendoc doctor [flags]\n\nRun credential and tooling self-checks.\n\nWith --platform, the named platforms' probe lines run even before `opendoc init`,\nso onboarding can probe reality before config exists.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(env.Stderr, "opendoc doctor: too many arguments\n")
		return ExitUsage
	}
	forceFeishu, forceNotion, perr := platforms.resolve()
	if perr != nil {
		fmt.Fprintf(env.Stderr, "opendoc doctor: %v\n", perr)
		return ExitUsage
	}

	l, err := layout.Resolve(*root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc doctor: %v\n", err)
		return ExitError
	}

	initialized := config.Exists(l.ConfigPath())
	cfg := config.Default()
	if initialized {
		loaded, lerr := config.Load(l.ConfigPath())
		if lerr != nil {
			fmt.Fprintf(env.Stderr, "opendoc doctor: %v\n", lerr)
			return ExitError
		}
		cfg = loaded
	}

	// Open the manifest read-only for the quota + docs-scope probes, only when it
	// already exists.
	var db *manifest.DB
	if fileExists(l.ManifestPath()) {
		opened, oerr := manifest.Open(l.ManifestPath())
		if oerr != nil {
			fmt.Fprintf(env.Stderr, "opendoc doctor: %v\n", oerr)
			return ExitError
		}
		db = opened
		defer db.Close()
	}

	// The engine is embedded (empty larkBin ⇒ self-exec); OPENDOC_LARK_CLI — from the
	// environment or the <root>/.internal/env file — overrides it with an
	// external lark-cli binary, and doctor then probes that binary like the
	// scheduled sync would use it.
	larkBin := config.ResolveEnv(larkCLIEnv, os.Getenv, l.EnvFilePath()).Value
	d := &doctor{
		cfg:           cfg,
		initialized:   initialized,
		root:          l.Root,
		db:            db,
		getenv:        os.Getenv,
		lookPath:      exec.LookPath,
		runner:        feishu.ExecRunner{Bin: larkBin},
		larkBin:       larkBin,
		engineVersion: embeddedEngineVersion,
		verifyNotion:  func(ctx context.Context, token string) error { return notion.NewClient(token, nil).Me(ctx) },
		searchNotion: func(ctx context.Context, token string) (int, error) {
			return notion.NewClient(token, nil).VisibleCount(ctx)
		},
		checkWritable: rootWritable,
		feishuSampleDoc: func() (string, bool) {
			if db == nil {
				return "", false
			}
			docs, derr := db.ListDocumentsByPlatform("feishu", "active")
			if derr != nil {
				return "", false
			}
			for _, doc := range docs {
				if (doc.Type == "docx" || doc.Type == "doc") && doc.ID != "" {
					return doc.ID, true
				}
			}
			return "", false
		},
		envFilePath: l.EnvFilePath(),
		forceFeishu: forceFeishu,
		forceNotion: forceNotion,
		now:         time.Now,
	}

	report := d.report(l.Root)
	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(env.Stderr, "opendoc doctor: %v\n", err)
			return ExitError
		}
	} else {
		printDoctor(env, report)
	}
	if !report.Initialized {
		return ExitNotInitialized
	}
	if report.OK {
		return ExitOK
	}
	return ExitError
}

// report runs every probe and folds the results into a doctorReport, with a
// bounded context so an unreachable network fails a probe rather than hanging.
func (d *doctor) report(root string) doctorReport {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	checks := d.run(ctx)
	ok := true
	for _, c := range checks {
		if c.Status == checkFail {
			ok = false
		}
	}
	return doctorReport{Root: root, Initialized: d.initialized, OK: ok, Checks: checks}
}

// run executes the probe list in a stable order: general (G0–G2), the Feishu line
// (F1–F5), then the Notion line (N1–N3). The full set of probe rows is always
// present (N/A ones become skips) so `--json` consumers see a stable schema.
func (d *doctor) run(ctx context.Context) []checkResult {
	var r []checkResult
	r = append(r, d.checkConfig())     // G0
	r = append(r, d.checkMirrorRoot()) // G1
	r = append(r, d.checkQuota())      // G2

	// Feishu line. A platform that is not configured is a report item (skips),
	// never a failure. When lark-cli is absent, downstream probes cannot
	// run and skip with a pointer to F1.
	if d.feishuConfigured() {
		f1 := d.checkLarkPresence(ctx)
		r = append(r, f1)
		if f1.Status == checkPass {
			r = append(r, d.checkLarkAuth(ctx))      // F2
			r = append(r, d.checkLarkScopes(ctx)...) // F3 (wiki/docs/drive)
			r = append(r, d.checkLarkFormat(ctx))    // F4
			r = append(r, d.checkMirrorScope(ctx))   // F5
		} else {
			r = append(r,
				skipCheck("F2", "lark-auth", "lark engine unavailable (see F1)"),
				skipCheck("F3", "lark-scope-wiki", "lark engine unavailable (see F1)"),
				skipCheck("F3", "lark-scope-docs", "lark engine unavailable (see F1)"),
				skipCheck("F3", "lark-scope-drive", "lark engine unavailable (see F1)"),
				skipCheck("F4", "lark-output-format", "lark engine unavailable (see F1)"),
				skipCheck("F5", "feishu-mirror-scope", "lark engine unavailable (see F1)"),
			)
		}
	} else {
		r = append(r,
			skipCheck("F1", "lark-engine", "feishu not configured"),
			skipCheck("F2", "lark-auth", "feishu not configured"),
			skipCheck("F3", "lark-scope-wiki", "feishu not configured"),
			skipCheck("F3", "lark-scope-docs", "feishu not configured"),
			skipCheck("F3", "lark-scope-drive", "feishu not configured"),
			skipCheck("F4", "lark-output-format", "feishu not configured"),
			skipCheck("F5", "feishu-mirror-scope", "feishu not configured"),
		)
	}

	// Notion line.
	r = append(r, d.checkNotionEnv())        // N1
	r = append(r, d.checkNotionToken(ctx))   // N2
	r = append(r, d.checkNotionVisible(ctx)) // N3
	return r
}

// General probes G0–G2.

// checkConfig is G0: config.toml existence. Its absence is a NORMAL diagnostic
// (NOT_INITIALIZED), not a crash — doctor runs fully without config.
func (d *doctor) checkConfig() checkResult {
	if d.initialized {
		return checkResult{ID: "G0", Name: "config", Status: checkPass, Detail: "config.toml present"}
	}
	return checkResult{ID: "G0", Name: "config", Status: checkFail, Code: "NOT_INITIALIZED",
		Detail: "config.toml not found; run onboarding (references/setup.md)"}
}

// checkMirrorRoot is G1: the mirror root must be resolvable and writable.
func (d *doctor) checkMirrorRoot() checkResult {
	if err := d.checkWritable(d.root); err != nil {
		return checkResult{ID: "G1", Name: "mirror-root", Status: checkFail, Code: "G1-UNWRITABLE",
			Detail: fmt.Sprintf("%s is not writable: %v", d.root, err)}
	}
	return checkResult{ID: "G1", Name: "mirror-root", Status: checkPass,
		Detail: fmt.Sprintf("%s is writable", d.root)}
}

// checkQuota is G2: it estimates the day's Feishu asset-download usage against the
// daily quota from sync_runs stats and WARNS (never fails — warnings do not block) when the
// remaining budget is low or exhausted. It skips when Feishu is not configured or
// no manifest exists.
func (d *doctor) checkQuota() checkResult {
	const id, name = "G2", "feishu-asset-quota"
	if !d.feishuConfigured() {
		return skipCheck(id, name, "feishu not configured")
	}
	if d.db == nil {
		return skipCheck(id, name, "no manifest yet (nothing to measure)")
	}
	now := d.now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := midnight.UTC().Format(time.RFC3339)
	statsJSON, err := d.db.StatsSince("feishu", since)
	if err != nil {
		return checkResult{ID: id, Name: name, Status: checkFail, Detail: fmt.Sprintf("read sync_runs stats: %v", err)}
	}
	used := 0
	for _, s := range statsJSON {
		if s == "" {
			continue
		}
		var st engine.Stats
		if json.Unmarshal([]byte(s), &st) == nil {
			used += st.AssetsDownloaded
		}
	}
	remaining := feishuAssetDailyQuota - used
	detail := fmt.Sprintf("assets downloaded today: %d / %d (remaining ~%d)", used, feishuAssetDailyQuota, remaining)
	if remaining <= 0 {
		return checkResult{ID: id, Name: name, Status: checkWarn, Code: "G2-QUOTA-LOW",
			Detail: detail + " — daily quota exhausted; image-heavy first mirror may finish next day (resumable)"}
	}
	if remaining < feishuAssetDailyQuota/quotaWarnFraction {
		return checkResult{ID: id, Name: name, Status: checkWarn, Code: "G2-QUOTA-LOW",
			Detail: detail + " — below 20% remaining; image-heavy first mirror may resume next day"}
	}
	return checkResult{ID: id, Name: name, Status: checkPass, Detail: detail}
}

// Feishu probes F1–F5.

// checkLarkPresence is F1. Default (embedded engine): always available, report
// the pinned module version. OPENDOC_LARK_CLI escape hatch (external binary): it
// must be on PATH, and its parseable version must be at least
// minLarkCLIVersion; an unparseable version does not fail F1 (F4 guards output
// drift).
func (d *doctor) checkLarkPresence(ctx context.Context) checkResult {
	const id, name = "F1", "lark-engine"
	if d.larkBin == "" {
		v := ""
		if d.engineVersion != nil {
			v = d.engineVersion()
		}
		if v == "" {
			return checkResult{ID: id, Name: name, Status: checkPass,
				Detail: "embedded engine (pinned version unavailable in this build; F4 guards drift)"}
		}
		return checkResult{ID: id, Name: name, Status: checkPass,
			Detail: fmt.Sprintf("embedded engine %s %s", engineModulePath, v)}
	}
	path, err := d.lookPath(d.larkBin)
	if err != nil {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "F1-MISSING",
			Detail: fmt.Sprintf("%q not found on PATH: %v", d.larkBin, err)}
	}
	raw := d.larkVersion(ctx)
	if raw == "" {
		return checkResult{ID: id, Name: name, Status: checkPass,
			Detail: fmt.Sprintf("found at %s (version output unavailable; F4 guards drift)", path)}
	}
	v, ok := parseSemver(raw)
	if !ok {
		return checkResult{ID: id, Name: name, Status: checkPass,
			Detail: fmt.Sprintf("found at %s (%q; version unparseable, F4 guards drift)", path, raw)}
	}
	if semverLess(v, minLarkCLIVersion) {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "F1-VERSION",
			Detail: fmt.Sprintf("found at %s but version %s is below the locked minimum %s", path, v, minLarkCLIVersion)}
	}
	return checkResult{ID: id, Name: name, Status: checkPass, Detail: fmt.Sprintf("found at %s (%s)", path, v)}
}

// larkVersion best-effort reads the lark-cli version via the Runner (`--version`).
// It returns "" when the probe fails.
func (d *doctor) larkVersion(ctx context.Context) string {
	out, err := d.runner.Run(ctx, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// checkLarkAuth is F2: authenticated? It consumes the shared user_info probe. A
// parseable ok:true envelope ⇒ authenticated. A parseable ok:false envelope ⇒
// F2-NOAUTH (not logged in or token expired). No parseable envelope ⇒ F2-NOCONFIG:
// lark-cli with no config/profile errors out before making the API call, emitting
// a non-envelope message rather than the {ok,...} wrapper (see notes below).
func (d *doctor) checkLarkAuth(ctx context.Context) checkResult {
	const id, name = "F2", "lark-auth"
	out, runErr := d.userInfo(ctx)
	env, perr := feishu.ParseEnvelope(out)
	if perr != nil {
		// No structured envelope. Most likely lark-cli has no config yet (has not
		// run config init): it fails before the API round-trip. A true output-format drift would
		// also land here; F4-DRIFT is the separate alarm, so we report NOCONFIG best-
		// effort and lean on F4.
		detail := "no valid lark-cli response envelope (likely no lark-cli config yet)"
		if runErr != nil {
			detail += ": " + firstLine(runErr.Error())
		}
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "F2-NOCONFIG",
			Detail: detail + " — run `opendoc lark-engine config init --new` then `opendoc lark-engine auth login --scope ...` (or `opendoc init`)"}
	}
	if env.OK {
		return checkResult{ID: id, Name: name, Status: checkPass,
			Detail: "authenticated (GET /open-apis/authen/v1/user_info)"}
	}
	// A well-formed ok:false envelope with a config error means lark-cli parsed the
	// request but has no app configured yet (verified live against lark-cli 1.0.69).
	// That is NOCONFIG, not NOAUTH — config init, not re-login, is the fix.
	if env.Error != nil && env.Error.Type == "config" {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "F2-NOCONFIG",
			Detail: "lark engine not configured (" + env.Error.String() + ") — run `opendoc lark-engine config init --new` then `opendoc lark-engine auth login --scope ...` (or `opendoc init`)"}
	}
	detail := "not authenticated"
	if env.Error != nil {
		if env.Error.IsAuthError() {
			detail = "auth token missing/expired"
		}
		detail += ": " + env.Error.String()
	}
	return checkResult{ID: id, Name: name, Status: checkFail, Code: "F2-NOAUTH",
		Detail: detail + " — run `opendoc lark-engine auth login --scope ...`"}
}

// checkLarkFormat is F4: the lark-cli output-format smoke check. It reuses the
// shared user_info response and validates it still parses to the {ok, data, error}
// envelope internal/feishu depends on. A drift fails with F4-DRIFT. When there is
// no response to test (empty output), F4 skips — that state is F2's to report.
func (d *doctor) checkLarkFormat(ctx context.Context) checkResult {
	const id, name = "F4", "lark-output-format"
	out, _ := d.userInfo(ctx)
	if len(strings.TrimSpace(string(out))) == 0 {
		return skipCheck(id, name, "no lark-cli response to smoke-test (see F2)")
	}
	if verr := feishu.CheckOutputFormat(out); verr != nil {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "F4-DRIFT", Detail: verr.Error()}
	}
	return checkResult{ID: id, Name: name, Status: checkPass, Detail: "lark-cli response envelope parses as expected"}
}

// checkLarkScopes is F3: one read-only probe per category the config actually uses
// (wiki spaces ⇒ wiki, drive folders / personal library ⇒ drive; docs piggybacks
// on any mirrored doc). A permission-denied response maps to F3-SCOPE-<category>.
func (d *doctor) checkLarkScopes(ctx context.Context) []checkResult {
	// Under a forced --platform feishu the config names no scope, so probe wiki and
	// drive unconditionally (fix 5); the docs probe still needs a mirrored doc.
	wikiUsed := d.forceFeishu || len(d.cfg.Feishu.WikiSpaces) > 0
	driveUsed := d.forceFeishu || len(d.cfg.Feishu.DriveFolders) > 0 || d.cfg.Feishu.IncludeMyLibrary
	return []checkResult{
		d.scopeProbe(ctx, "wiki", wikiUsed, "GET", "/open-apis/wiki/v2/spaces"),
		d.scopeDocsProbe(ctx),
		d.scopeProbe(ctx, "drive", driveUsed, "GET", "/open-apis/drive/v1/files"),
	}
}

// scopeProbe runs a single read-only category probe and classifies the response.
// It skips when the category is not in the mirror scope.
func (d *doctor) scopeProbe(ctx context.Context, category string, used bool, method, path string) checkResult {
	name := "lark-scope-" + category
	if !used {
		return skipCheck("F3", name, category+" not in mirror scope")
	}
	out, err := d.runner.Run(ctx, "api", method, path)
	return classifyScope(name, category, out, err)
}

// scopeDocsProbe is the F3 docs-category probe. It piggybacks on a mirrored Feishu
// doc's metadata read; with no mirrored doc yet it skips (docs scope unverified).
func (d *doctor) scopeDocsProbe(ctx context.Context) checkResult {
	const name, category = "lark-scope-docs", "docs"
	docID, ok := d.feishuSampleDoc()
	if !ok {
		return skipCheck("F3", name, "no mirrored Feishu doc to probe (docs scope unverified)")
	}
	out, err := d.runner.Run(ctx, "api", "GET", "/open-apis/docx/v1/documents/"+docID)
	return classifyScope(name, category, out, err)
}

// classifyScope maps a scope-probe response to a checkResult: ok ⇒ pass; a
// permission error ⇒ F3-SCOPE-<category>; any other structured error or an
// unparseable response ⇒ a plain fail with the raw detail (F4 alarms drift).
func classifyScope(name, category string, out []byte, runErr error) checkResult {
	env, perr := feishu.ParseEnvelope(out)
	if perr != nil {
		detail := "unexpected lark-cli output (see F4)"
		if runErr != nil {
			detail += ": " + firstLine(runErr.Error())
		}
		return checkResult{ID: "F3", Name: name, Status: checkFail, Detail: detail}
	}
	if env.OK {
		return checkResult{ID: "F3", Name: name, Status: checkPass, Detail: "read-only " + category + " probe succeeded"}
	}
	if env.Error.IsPermissionError() {
		return checkResult{ID: "F3", Name: name, Status: checkFail, Code: "F3-SCOPE-" + category,
			Detail: "insufficient scope for " + category + ": " + env.Error.String() + " — grant the scope, then re-run `opendoc lark-engine auth login`"}
	}
	return checkResult{ID: "F3", Name: name, Status: checkFail, Detail: category + " probe error: " + env.Error.String()}
}

// checkMirrorScope is F5: every configured wiki space / drive folder must still be
// accessible. Each stale/inaccessible id contributes an F5-STALE-<id> code; the
// single F5 row joins them (space-separated in Code, listed in Detail) so the JSON
// stays a stable one-entry-per-probe shape while still naming each id.
func (d *doctor) checkMirrorScope(ctx context.Context) checkResult {
	const id, name = "F5", "feishu-mirror-scope"
	type target struct{ kind, id string }
	var targets []target
	for _, s := range d.cfg.Feishu.WikiSpaces {
		targets = append(targets, target{"wiki", s})
	}
	for _, f := range d.cfg.Feishu.DriveFolders {
		targets = append(targets, target{"drive", f})
	}
	if len(targets) == 0 {
		return skipCheck(id, name, "no wiki spaces / drive folders configured")
	}
	var stale, codes []string
	for _, t := range targets {
		var out []byte
		var err error
		if t.kind == "wiki" {
			out, err = d.runner.Run(ctx, "api", "GET", "/open-apis/wiki/v2/spaces/"+t.id)
		} else {
			out, err = d.runner.Run(ctx, "api", "GET", "/open-apis/drive/v1/files?folder_token="+t.id)
		}
		if !targetAccessible(out, err) {
			stale = append(stale, t.kind+":"+t.id)
			codes = append(codes, "F5-STALE-"+t.id)
		}
	}
	if len(stale) > 0 {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: strings.Join(codes, " "),
			Detail: "inaccessible mirror targets: " + strings.Join(stale, ", ") + " (reauthorize or remove from config)"}
	}
	return checkResult{ID: id, Name: name, Status: checkPass, Detail: "all configured wiki spaces / drive folders accessible"}
}

// targetAccessible reports whether an F5 target probe returned an ok envelope. A
// parse failure or ok:false both count as inaccessible.
func targetAccessible(out []byte, runErr error) bool {
	env, perr := feishu.ParseEnvelope(out)
	if perr != nil {
		return false
	}
	return env.OK
}

// Notion probes N1–N3.

// checkNotionEnv is N1: the token must be resolvable from the process environment
// or the <root>/.internal/env fallback file (fix 4). It skips when Notion is not
// configured. Detail names the source; a value present but backed by a
// world/group-readable env file warns (never fails) with a chmod-600 pointer.
func (d *doctor) checkNotionEnv() checkResult {
	const id, name = "N1", "notion-token-env"
	if !d.notionEnabled() {
		return skipCheck(id, name, "notion not configured (no [notion] token_env)")
	}
	res := d.resolveToken()
	if res.Value == "" {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "N1-UNSET",
			Detail: fmt.Sprintf("token env var %s is set neither in the environment nor in %s", res.Name, d.envFilePath)}
	}
	source := "from environment"
	if res.Source == config.SourceFile {
		source = "from " + d.envFilePath
	}
	if res.FileLoose {
		return checkResult{ID: id, Name: name, Status: checkWarn, Code: "N1-ENVFILE-PERMS",
			Detail: fmt.Sprintf("token env var %s resolved (%s), but %s is group/world-readable — run `chmod 600 %s`",
				res.Name, source, d.envFilePath, d.envFilePath)}
	}
	return checkResult{ID: id, Name: name, Status: checkPass,
		Detail: fmt.Sprintf("token env var %s resolved (%s)", res.Name, source)}
}

// checkNotionToken is N2: the token must be valid via GET /v1/users/me. It skips
// when Notion is not configured or the env var is empty (N1 owns that).
func (d *doctor) checkNotionToken(ctx context.Context) checkResult {
	const id, name = "N2", "notion-token"
	if !d.notionEnabled() {
		return skipCheck(id, name, "notion not configured")
	}
	token := d.resolveToken().Value
	if token == "" {
		return skipCheck(id, name, "token env var empty (see N1)")
	}
	if err := d.verifyNotion(ctx, token); err != nil {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "N2-INVALID",
			Detail: fmt.Sprintf("GET /v1/users/me failed: %v", err)}
	}
	return checkResult{ID: id, Name: name, Status: checkPass, Detail: "token valid (GET /v1/users/me)"}
}

// checkNotionVisible is N3: how many pages/databases the integration can see. Zero
// visible objects with a valid token is N3-EMPTY — the integration is connected to
// nothing, so sync would produce nothing; this is indistinguishable from a healthy
// empty workspace at the API level and must be surfaced explicitly.
func (d *doctor) checkNotionVisible(ctx context.Context) checkResult {
	const id, name = "N3", "notion-visible"
	if !d.notionEnabled() {
		return skipCheck(id, name, "notion not configured")
	}
	token := d.resolveToken().Value
	if token == "" {
		return skipCheck(id, name, "token env var empty (see N1)")
	}
	n, err := d.searchNotion(ctx, token)
	if err != nil {
		return checkResult{ID: id, Name: name, Status: checkFail,
			Detail: fmt.Sprintf("POST /v1/search failed: %v", err)}
	}
	if n == 0 {
		return checkResult{ID: id, Name: name, Status: checkFail, Code: "N3-EMPTY",
			Detail: "token valid but 0 pages/databases visible — the integration is connected to nothing; sync would produce nothing (connect pages in Notion)"}
	}
	return checkResult{ID: id, Name: name, Status: checkPass,
		Detail: fmt.Sprintf("visible: %d pages/databases (first search page)", n)}
}

// userInfo runs the single read-only lark-cli user_info invocation, memoized so
// F2 and F4 share one call.
func (d *doctor) userInfo(ctx context.Context) ([]byte, error) {
	if !d.userInfoDone {
		d.userInfoOut, d.userInfoErr = d.runner.Run(ctx, feishu.SmokeProbeArgs...)
		d.userInfoDone = true
	}
	return d.userInfoOut, d.userInfoErr
}

// feishuConfigured reports whether the Feishu probe line should run: either the
// config names a Feishu source to mirror (the adapter's own Configured predicate)
// or --platform forced it (fix 5).
func (d *doctor) feishuConfigured() bool {
	if d.forceFeishu {
		return true
	}
	return feishu.NewAdapter(d.cfg.Feishu, d.runner, 0).Configured()
}

// notionEnabled reports whether the Notion probe line should run: either the
// config names a token env var or --platform forced it (fix 5).
func (d *doctor) notionEnabled() bool {
	return d.forceNotion || d.cfg.Notion.Enabled()
}

// notionTokenEnv is the environment variable that carries the Notion token. It is
// the configured value, or the DefaultNotionTokenEnv fallback (NOTION_TOKEN) when
// the config names none — the case that arises under a forced --platform notion on
// an uninitialized root (fix 5).
func (d *doctor) notionTokenEnv() string {
	if d.cfg.Notion.TokenEnv != "" {
		return d.cfg.Notion.TokenEnv
	}
	return config.DefaultNotionTokenEnv
}

// resolveToken resolves the Notion token through the environment then the
// <root>/.internal/env fallback file (fix 4), so N1/N2/N3 honour both channels.
func (d *doctor) resolveToken() config.EnvResolution {
	return config.ResolveEnv(d.notionTokenEnv(), d.getenv, d.envFilePath)
}

// platformList is a repeatable/comma-separated --platform flag value.
type platformList []string

func (p *platformList) String() string { return strings.Join(*p, ",") }

// Set appends comma-separated platform names from one flag occurrence.
func (p *platformList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if s := strings.TrimSpace(part); s != "" {
			*p = append(*p, s)
		}
	}
	return nil
}

// resolve folds the --platform values into force flags, rejecting unknown names
// with a usage error (exit 2).
func (p platformList) resolve() (feishu, notion bool, err error) {
	for _, name := range p {
		switch strings.ToLower(name) {
		case "feishu":
			feishu = true
		case "notion":
			notion = true
		default:
			return false, false, fmt.Errorf("unknown platform %q (want feishu|notion)", name)
		}
	}
	return feishu, notion, nil
}

// skipCheck builds a skip result (no code).
func skipCheck(id, name, detail string) checkResult {
	return checkResult{ID: id, Name: name, Status: checkSkip, Detail: detail}
}

// firstLine returns s up to the first newline, trimmed — keeps a multi-line
// stderr from flooding a one-line probe detail.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// semverRe extracts a dotted numeric version (major[.minor[.patch]]) from arbitrary
// lark-cli --version output (e.g. "lark-cli version 1.0.69").
var semverRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// parseSemver pulls the version numbers out of a --version string. ok is false
// when no dotted version is present (then F1 does not fail on version).
func parseSemver(s string) (string, bool) {
	m := semverRe.FindString(s)
	if m == "" {
		return "", false
	}
	return m, true
}

// semverLess reports whether version a is strictly lower than version b. Both are
// dotted numeric strings; missing components compare as 0.
func semverLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		an, bn := 0, 0
		if i < len(as) {
			an, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bn, _ = strconv.Atoi(bs[i])
		}
		if an != bn {
			return an < bn
		}
	}
	return false
}

// rootWritable reports whether the mirror root is writable without creating it: it
// walks up to the nearest existing ancestor directory and probes it with a
// temp-file create/remove. This lets G1 verify writability even before `opendoc init`
// materialises the root, without leaving anything behind.
func rootWritable(root string) error {
	dir := root
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists but is not a directory", dir)
			}
			f, cerr := os.CreateTemp(dir, ".opendoc-doctor-*")
			if cerr != nil {
				return cerr
			}
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("no existing ancestor directory for %s", root)
		}
		dir = parent
	}
}

// printDoctor renders the report as one line per probe plus a summary line. Each
// line shows the probe id, name, failure code (when any), and detail.
func printDoctor(env Env, r doctorReport) {
	for _, c := range r.Checks {
		code := ""
		if c.Code != "" {
			code = " (" + c.Code + ")"
		}
		fmt.Fprintf(env.Stdout, "[%-4s] %-3s %-20s%s %s\n", c.Status, c.ID, c.Name, code, c.Detail)
	}
	overall := "ok"
	switch {
	case !r.Initialized:
		overall = "NOT_INITIALIZED"
	case !r.OK:
		overall = "FAIL"
	}
	fmt.Fprintf(env.Stdout, "overall: %s\n", overall)
}
