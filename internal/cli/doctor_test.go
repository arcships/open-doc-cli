package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/feishu"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// Canned lark-cli response envelopes for the probes.
const (
	envOK       = `{"ok":true,"data":{"user_info":{"name":"bot"}}}`
	envAuthFail = `{"ok":false,"error":{"type":"auth","code":99991663,"message":"token expired"}}`
	envPermFail = `{"ok":false,"error":{"type":"permission","code":99991672,"message":"no permission"}}`
	envOtherErr = `{"ok":false,"error":{"type":"server","code":50000,"message":"boom"}}`
	envTampered = `{"success":true,"payload":{"user":"bot"}}` // renamed envelope fields (drift)
	// The real unconfigured-lark-cli envelope (verified live, lark-cli 1.0.69): a
	// well-formed ok:false whose error is a config error ⇒ F2-NOCONFIG, not NOAUTH.
	envNotConfigured = `{"ok":false,"identity":"bot","error":{"type":"config","subtype":"not_configured","message":"lark-cli is not configured"}}`
)

// fakeRunner is a feishu.Runner that answers `--version` with a fixed string and
// every other invocation with a canned body/error, so the doctor probes run with
// no real binary. A single response suffices for the per-probe unit tests, which
// each exercise exactly one endpoint.
type fakeRunner struct {
	version string
	out     []byte
	err     error
}

func (f fakeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		if f.version == "" {
			return nil, errors.New("no version")
		}
		return []byte(f.version), nil
	}
	return f.out, f.err
}

func (f fakeRunner) RunInDir(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return f.Run(ctx, args...)
}

// respond builds a fakeRunner returning body (+err) for any non-version call.
func respond(version, body string, err error) fakeRunner {
	return fakeRunner{version: version, out: []byte(body), err: err}
}

// feishuCfg is a config with Feishu configured (one wiki space) and Notion off.
func feishuCfg() config.Config {
	c := config.Default()
	c.Feishu.WikiSpaces = []string{"S1"}
	return c
}

// notionCfg is a config with Notion configured and Feishu off.
func notionCfg() config.Config {
	c := config.Default()
	c.Notion.TokenEnv = "NOTION_TOKEN"
	return c
}

func baseDoctor(cfg config.Config, runner feishu.Runner) *doctor {
	return &doctor{
		cfg:             cfg,
		initialized:     true,
		root:            "/mirror",
		getenv:          func(string) string { return "" },
		lookPath:        func(string) (string, error) { return "/usr/local/bin/lark-cli", nil },
		runner:          runner,
		larkBin:         "lark-cli", // escape-hatch mode; embedded-mode probes override to ""
		verifyNotion:    func(context.Context, string) error { return errors.New("verifyNotion should not be called") },
		searchNotion:    func(context.Context, string) (int, error) { return 0, errors.New("searchNotion should not be called") },
		checkWritable:   func(string) error { return nil },
		feishuSampleDoc: func() (string, bool) { return "", false },
		now:             func() time.Time { return time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC) },
	}
}

// wantCode asserts the probe's status and failure code.
func wantCode(t *testing.T, got checkResult, status, code string) {
	t.Helper()
	if got.Status != status || got.Code != code {
		t.Fatalf("probe %s/%s = status %q code %q, want status %q code %q; detail=%s",
			got.ID, got.Name, got.Status, got.Code, status, code, got.Detail)
	}
}

// ---- G0/G1 ----

func TestDoctorG0Config(t *testing.T) {
	d := baseDoctor(config.Default(), fakeRunner{})
	wantCode(t, d.checkConfig(), checkPass, "")

	d.initialized = false
	wantCode(t, d.checkConfig(), checkFail, "NOT_INITIALIZED")
}

func TestDoctorG1MirrorRoot(t *testing.T) {
	d := baseDoctor(config.Default(), fakeRunner{})
	wantCode(t, d.checkMirrorRoot(), checkPass, "")

	d.checkWritable = func(string) error { return errors.New("read-only file system") }
	wantCode(t, d.checkMirrorRoot(), checkFail, "G1-UNWRITABLE")
}

// TestRootWritable exercises the real writability probe against a temp dir (pass)
// and a path whose ancestor is a file (fail), with no fakes.
func TestRootWritable(t *testing.T) {
	dir := t.TempDir()
	if err := rootWritable(filepath.Join(dir, "does-not-exist-yet")); err != nil {
		t.Errorf("rootWritable on a writable-ancestor path = %v, want nil", err)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rootWritable(filepath.Join(file, "sub")); err == nil {
		t.Errorf("rootWritable under a regular file = nil, want error")
	}
}

// ---- G2 quota (warn, never fails) ----

func TestDoctorG2QuotaWarn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	defer db.Close()
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	// 9500 of 10000 downloaded today ⇒ remaining 500 (< 20%) ⇒ warn.
	if _, err := db.InsertSyncRun(manifest.SyncRun{
		Platform: "feishu", StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Hour),
		Stats: `{"assets_downloaded":9500}`,
	}); err != nil {
		t.Fatal(err)
	}
	d := baseDoctor(feishuCfg(), fakeRunner{})
	d.db = db
	d.now = func() time.Time { return now }

	got := d.checkQuota()
	wantCode(t, got, checkWarn, "G2-QUOTA-LOW")

	// A warn must NOT fail the overall report (warnings do not block).
	rep := doctorReport{Checks: []checkResult{got}, OK: true}
	for _, c := range rep.Checks {
		if c.Status == checkFail {
			rep.OK = false
		}
	}
	if !rep.OK {
		t.Errorf("a warn probe flipped report.OK to false; warn must not affect the exit code")
	}
}

func TestDoctorG2QuotaExhaustedWarn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	defer db.Close()
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	if _, err := db.InsertSyncRun(manifest.SyncRun{
		Platform: "feishu", StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Hour),
		Stats: `{"assets_downloaded":12000}`,
	}); err != nil {
		t.Fatal(err)
	}
	d := baseDoctor(feishuCfg(), fakeRunner{})
	d.db = db
	d.now = func() time.Time { return now }
	// Fully exhausted is still a warn, not a fail (design defines only G2-QUOTA-LOW).
	wantCode(t, d.checkQuota(), checkWarn, "G2-QUOTA-LOW")
}

func TestDoctorG2QuotaPassAndSkip(t *testing.T) {
	// No manifest ⇒ skip.
	d := baseDoctor(feishuCfg(), fakeRunner{})
	d.db = nil
	if got := d.checkQuota(); got.Status != checkSkip {
		t.Errorf("quota (no manifest) = %q, want skip", got.Status)
	}

	// Plenty of budget ⇒ pass.
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	defer db.Close()
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	if _, err := db.InsertSyncRun(manifest.SyncRun{
		Platform: "feishu", StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Hour),
		Stats: `{"assets_downloaded":40}`,
	}); err != nil {
		t.Fatal(err)
	}
	d.db = db
	d.now = func() time.Time { return now }
	got := d.checkQuota()
	if got.Status != checkPass || !strings.Contains(got.Detail, "40") {
		t.Errorf("quota = %q detail=%q, want pass mentioning 40", got.Status, got.Detail)
	}
}

// ---- F1 engine presence + version ----

func TestDoctorF1Embedded(t *testing.T) {
	// Default mode: embedded engine, no external binary — always pass, reporting
	// the go.mod-pinned version from build info.
	d := baseDoctor(feishuCfg(), fakeRunner{})
	d.larkBin = ""
	d.engineVersion = func() string { return "v1.0.70" }
	got := d.checkLarkPresence(context.Background())
	if got.Status != checkPass || !strings.Contains(got.Detail, "v1.0.70") {
		t.Errorf("embedded F1 = %q detail=%q, want pass mentioning v1.0.70", got.Status, got.Detail)
	}

	// Version unavailable (e.g. test binary without the engine linked) ⇒ still
	// pass; F4 guards drift.
	d.engineVersion = func() string { return "" }
	wantCode(t, d.checkLarkPresence(context.Background()), checkPass, "")
}

func TestDoctorF1(t *testing.T) {
	// The remaining F1 cases exercise the OPENDOC_LARK_CLI escape hatch (external
	// binary probed on PATH with the version floor).
	// Found + current version ⇒ pass.
	d := baseDoctor(feishuCfg(), fakeRunner{version: "lark-cli version 1.0.69"})
	wantCode(t, d.checkLarkPresence(context.Background()), checkPass, "")

	// Missing on PATH ⇒ F1-MISSING.
	d.lookPath = func(string) (string, error) { return "", errors.New("not found in $PATH") }
	wantCode(t, d.checkLarkPresence(context.Background()), checkFail, "F1-MISSING")

	// Below the locked minimum ⇒ F1-VERSION.
	d = baseDoctor(feishuCfg(), fakeRunner{version: "lark-cli version 1.0.50"})
	wantCode(t, d.checkLarkPresence(context.Background()), checkFail, "F1-VERSION")

	// Unparseable version ⇒ pass (F4 guards drift), not F1-VERSION.
	d = baseDoctor(feishuCfg(), fakeRunner{version: "dev-build"})
	if got := d.checkLarkPresence(context.Background()); got.Status != checkPass {
		t.Errorf("unparseable version = %q, want pass; detail=%s", got.Status, got.Detail)
	}
}

// ---- F2 auth (shares the user_info probe with F4) ----

func TestDoctorF2Auth(t *testing.T) {
	// ok envelope ⇒ authenticated (pass).
	d := baseDoctor(feishuCfg(), respond("1.0.69", envOK, nil))
	wantCode(t, d.checkLarkAuth(context.Background()), checkPass, "")

	// ok:false auth error ⇒ F2-NOAUTH.
	d = baseDoctor(feishuCfg(), respond("1.0.69", envAuthFail, errors.New("exit status 1")))
	wantCode(t, d.checkLarkAuth(context.Background()), checkFail, "F2-NOAUTH")

	// No parseable envelope (lark-cli has no config yet) ⇒ F2-NOCONFIG.
	d = baseDoctor(feishuCfg(), respond("1.0.69", "no config found; run `lark-cli config init`", errors.New("exit status 1")))
	wantCode(t, d.checkLarkAuth(context.Background()), checkFail, "F2-NOCONFIG")

	// A well-formed ok:false whose error.type is "config" (the real unconfigured
	// lark-cli 1.0.69 envelope) ⇒ F2-NOCONFIG, NOT the NOAUTH it used to misfire.
	d = baseDoctor(feishuCfg(), respond("1.0.69", envNotConfigured, errors.New("exit status 1")))
	wantCode(t, d.checkLarkAuth(context.Background()), checkFail, "F2-NOCONFIG")
}

// TestDoctorUserInfoSharedOnce verifies F2 and F4 reuse a single lark-cli call.
func TestDoctorUserInfoSharedOnce(t *testing.T) {
	calls := 0
	counting := countingRunner{version: "1.0.69", body: envOK, count: &calls}
	d := baseDoctor(feishuCfg(), counting)
	_ = d.checkLarkAuth(context.Background())
	_ = d.checkLarkFormat(context.Background())
	if calls != 1 {
		t.Errorf("user_info invoked %d times, want 1 (F2 and F4 must share the call)", calls)
	}
}

type countingRunner struct {
	version string
	body    string
	count   *int
}

func (r countingRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte(r.version), nil
	}
	*r.count++
	return []byte(r.body), nil
}
func (r countingRunner) RunInDir(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return r.Run(ctx, args...)
}

// ---- F3 scope ----

func TestDoctorF3ScopeWiki(t *testing.T) {
	d := baseDoctor(feishuCfg(), respond("1.0.69", envPermFail, errors.New("exit status 1")))
	results := d.checkLarkScopes(context.Background())
	wiki := findByName(results, "lark-scope-wiki")
	wantCode(t, wiki, checkFail, "F3-SCOPE-wiki")
	// The uninvolved categories skip (no drive folders, no sample doc).
	if got := findByName(results, "lark-scope-drive"); got.Status != checkSkip {
		t.Errorf("drive scope = %q, want skip", got.Status)
	}
}

func TestDoctorF3ScopeDrive(t *testing.T) {
	cfg := config.Default()
	cfg.Feishu.DriveFolders = []string{"fld1"}
	d := baseDoctor(cfg, respond("1.0.69", envPermFail, errors.New("exit status 1")))
	wantCode(t, findByName(d.checkLarkScopes(context.Background()), "lark-scope-drive"), checkFail, "F3-SCOPE-drive")
}

func TestDoctorF3ScopeDocs(t *testing.T) {
	d := baseDoctor(config.Default(), respond("1.0.69", envPermFail, errors.New("exit status 1")))
	d.feishuSampleDoc = func() (string, bool) { return "docXYZ", true }
	wantCode(t, findByName(d.checkLarkScopes(context.Background()), "lark-scope-docs"), checkFail, "F3-SCOPE-docs")

	// No sample doc ⇒ docs scope skips.
	d.feishuSampleDoc = func() (string, bool) { return "", false }
	if got := findByName(d.checkLarkScopes(context.Background()), "lark-scope-docs"); got.Status != checkSkip {
		t.Errorf("docs scope (no sample) = %q, want skip", got.Status)
	}
}

func TestDoctorF3ScopePassAndOtherError(t *testing.T) {
	// ok envelope ⇒ pass.
	d := baseDoctor(feishuCfg(), respond("1.0.69", envOK, nil))
	wantCode(t, findByName(d.checkLarkScopes(context.Background()), "lark-scope-wiki"), checkPass, "")

	// Non-permission error ⇒ plain fail (no F3-SCOPE code), raw detail.
	d = baseDoctor(feishuCfg(), respond("1.0.69", envOtherErr, errors.New("exit status 1")))
	got := findByName(d.checkLarkScopes(context.Background()), "lark-scope-wiki")
	if got.Status != checkFail || got.Code != "" {
		t.Errorf("non-permission scope error = status %q code %q, want fail with empty code", got.Status, got.Code)
	}
}

// ---- F4 output-format smoke ----

func TestDoctorF4Format(t *testing.T) {
	// Tampered envelope ⇒ F4-DRIFT.
	d := baseDoctor(feishuCfg(), respond("1.0.69", envTampered, nil))
	wantCode(t, d.checkLarkFormat(context.Background()), checkFail, "F4-DRIFT")

	// Valid envelope ⇒ pass.
	d = baseDoctor(feishuCfg(), respond("1.0.69", envOK, nil))
	wantCode(t, d.checkLarkFormat(context.Background()), checkPass, "")

	// ok:false auth envelope is still well-formed ⇒ format pass.
	d = baseDoctor(feishuCfg(), respond("1.0.69", envAuthFail, errors.New("exit status 1")))
	wantCode(t, d.checkLarkFormat(context.Background()), checkPass, "")

	// No output ⇒ skip (that state is F2's to report, not drift).
	d = baseDoctor(feishuCfg(), respond("1.0.69", "", errors.New("exec: not found")))
	if got := d.checkLarkFormat(context.Background()); got.Status != checkSkip {
		t.Errorf("format (no output) = %q, want skip", got.Status)
	}
}

// ---- F5 mirror scope validity ----

func TestDoctorF5Stale(t *testing.T) {
	cfg := config.Default()
	cfg.Feishu.WikiSpaces = []string{"S1"}
	cfg.Feishu.DriveFolders = []string{"fld1"}
	d := baseDoctor(cfg, respond("1.0.69", envPermFail, errors.New("exit status 1")))
	got := d.checkMirrorScope(context.Background())
	if got.Status != checkFail {
		t.Fatalf("F5 = %q, want fail; detail=%s", got.Status, got.Detail)
	}
	for _, want := range []string{"F5-STALE-S1", "F5-STALE-fld1"} {
		if !strings.Contains(got.Code, want) {
			t.Errorf("F5 code %q missing %q", got.Code, want)
		}
	}
}

func TestDoctorF5AllAccessible(t *testing.T) {
	d := baseDoctor(feishuCfg(), respond("1.0.69", envOK, nil))
	if got := d.checkMirrorScope(context.Background()); got.Status != checkPass {
		t.Errorf("F5 (all ok) = %q, want pass; detail=%s", got.Status, got.Detail)
	}
	// No targets configured ⇒ skip.
	d = baseDoctor(config.Default(), respond("1.0.69", envOK, nil))
	if got := d.checkMirrorScope(context.Background()); got.Status != checkSkip {
		t.Errorf("F5 (no targets) = %q, want skip", got.Status)
	}
}

// ---- N1/N2/N3 ----

func TestDoctorN1Env(t *testing.T) {
	// Not configured ⇒ skip.
	d := baseDoctor(config.Default(), fakeRunner{})
	if got := d.checkNotionEnv(); got.Status != checkSkip {
		t.Errorf("N1 (unconfigured) = %q, want skip", got.Status)
	}
	// Configured, env empty ⇒ N1-UNSET.
	d = baseDoctor(notionCfg(), fakeRunner{})
	wantCode(t, d.checkNotionEnv(), checkFail, "N1-UNSET")
	// Configured, env set ⇒ pass.
	d.getenv = func(string) string { return "secret" }
	wantCode(t, d.checkNotionEnv(), checkPass, "")
	if got := d.checkNotionEnv(); !strings.Contains(got.Detail, "from environment") {
		t.Errorf("N1 (env source) detail = %q, want mention of 'from environment'", got.Detail)
	}
}

// TestDoctorN1EnvFileFallback covers fix 4: N1 passes when the token is absent
// from the environment but present in <root>/.internal/env, names that source,
// and warns (never fails) when the env file has loose permissions.
func TestDoctorN1EnvFileFallback(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath, []byte(`export NOTION_TOKEN="ntn_from_file"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := baseDoctor(notionCfg(), fakeRunner{})
	d.getenv = func(string) string { return "" } // not in the environment
	d.envFilePath = envPath

	got := d.checkNotionEnv()
	wantCode(t, got, checkPass, "")
	if !strings.Contains(got.Detail, envPath) {
		t.Errorf("N1 (file source) detail = %q, want mention of %q", got.Detail, envPath)
	}

	// N2/N3 must reach the fallback token too.
	d.verifyNotion = func(_ context.Context, tok string) error {
		if tok != "ntn_from_file" {
			t.Errorf("N2 received token %q, want ntn_from_file", tok)
		}
		return nil
	}
	wantCode(t, d.checkNotionToken(context.Background()), checkPass, "")

	// Loose permissions ⇒ warn (not fail).
	if err := os.Chmod(envPath, 0o644); err != nil {
		t.Fatal(err)
	}
	got = d.checkNotionEnv()
	if got.Status != checkWarn {
		t.Errorf("N1 (loose env file) = %q, want warn; detail=%s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "chmod 600") {
		t.Errorf("N1 warn detail = %q, want a chmod 600 pointer", got.Detail)
	}

	// Neither channel ⇒ N1-UNSET naming both channels.
	if err := os.Remove(envPath); err != nil {
		t.Fatal(err)
	}
	got = d.checkNotionEnv()
	wantCode(t, got, checkFail, "N1-UNSET")
	if !strings.Contains(got.Detail, envPath) {
		t.Errorf("N1-UNSET detail = %q, want mention of the env file path", got.Detail)
	}
}

// TestDoctorPlatformOverride covers fix 5: --platform forces a platform's probes
// to run on an uninitialized root (still exit 3 / NOT_INITIALIZED), and an unknown
// platform value is a usage error.
func TestDoctorPlatformOverride(t *testing.T) {
	// Unknown platform ⇒ usage error (exit 2).
	env, _, errb := newEnv("doctor", "--root", filepath.Join(t.TempDir(), "m"), "--platform", "bogus")
	if code := Run(env); code != ExitUsage {
		t.Fatalf("doctor --platform bogus = %d, want %d; stderr=%s", code, ExitUsage, errb.String())
	}

	// Forced probes on an uninitialized root: N1 runs (real probe result, not a
	// skip) while G0 still reports NOT_INITIALIZED and the command exits 3.
	root := filepath.Join(t.TempDir(), "mirror")
	env, out, errb := newEnv("doctor", "--root", root, "--platform", "notion", "--json")
	if code := Run(env); code != ExitNotInitialized {
		t.Fatalf("doctor --platform notion (uninit) = %d, want %d; stderr=%s", code, ExitNotInitialized, errb.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor JSON invalid: %v; got %q", err, out.String())
	}
	if rep.Initialized {
		t.Errorf("report.Initialized = true, want false")
	}
	n1 := findByName(rep.Checks, "notion-token-env")
	if n1.Status == checkSkip {
		t.Errorf("N1 skipped under --platform notion; want a real probe result. detail=%s", n1.Detail)
	}
}

// TestDoctorPlatformForcesProbes verifies the force flags flip the gating
// predicates on a doctor built with fakes (no real config, no binary).
func TestDoctorPlatformForcesProbes(t *testing.T) {
	d := baseDoctor(config.Default(), respond("1.0.69", envOK, nil))
	d.forceFeishu = true
	d.forceNotion = true

	if !d.feishuConfigured() {
		t.Errorf("feishuConfigured() = false under forceFeishu, want true")
	}
	if !d.notionEnabled() {
		t.Errorf("notionEnabled() = false under forceNotion, want true")
	}
	if got := d.notionTokenEnv(); got != config.DefaultNotionTokenEnv {
		t.Errorf("notionTokenEnv() = %q, want default %q", got, config.DefaultNotionTokenEnv)
	}
	// F3 forces both wiki and drive category probes (docs still needs a sample doc).
	results := d.checkLarkScopes(context.Background())
	if got := findByName(results, "lark-scope-wiki"); got.Status != checkPass {
		t.Errorf("forced wiki scope = %q, want pass", got.Status)
	}
	if got := findByName(results, "lark-scope-drive"); got.Status != checkPass {
		t.Errorf("forced drive scope = %q, want pass", got.Status)
	}
}

func TestDoctorN2Token(t *testing.T) {
	d := baseDoctor(notionCfg(), fakeRunner{})
	d.getenv = func(string) string { return "secret" }

	// Valid token ⇒ pass.
	d.verifyNotion = func(context.Context, string) error { return nil }
	wantCode(t, d.checkNotionToken(context.Background()), checkPass, "")

	// Bad token ⇒ N2-INVALID.
	d.verifyNotion = func(context.Context, string) error { return errors.New("notion api status 401") }
	wantCode(t, d.checkNotionToken(context.Background()), checkFail, "N2-INVALID")

	// Env empty ⇒ skip (N1 owns that).
	d.getenv = func(string) string { return "" }
	if got := d.checkNotionToken(context.Background()); got.Status != checkSkip {
		t.Errorf("N2 (empty env) = %q, want skip", got.Status)
	}
}

func TestDoctorN3Visible(t *testing.T) {
	d := baseDoctor(notionCfg(), fakeRunner{})
	d.getenv = func(string) string { return "secret" }

	// Zero visible ⇒ N3-EMPTY.
	d.searchNotion = func(context.Context, string) (int, error) { return 0, nil }
	wantCode(t, d.checkNotionVisible(context.Background()), checkFail, "N3-EMPTY")

	// Non-zero ⇒ pass with a count in the detail.
	d.searchNotion = func(context.Context, string) (int, error) { return 7, nil }
	got := d.checkNotionVisible(context.Background())
	if got.Status != checkPass || !strings.Contains(got.Detail, "7") {
		t.Errorf("N3 (7 visible) = %q detail=%q, want pass mentioning 7", got.Status, got.Detail)
	}

	// Search error ⇒ fail (surfaced, not hidden).
	d.searchNotion = func(context.Context, string) (int, error) { return 0, errors.New("offline") }
	if got := d.checkNotionVisible(context.Background()); got.Status != checkFail {
		t.Errorf("N3 (search error) = %q, want fail", got.Status)
	}
}

// ---- Full-command wiring ----

// TestDoctorNotInitialized: doctor runs the full report without config and exits
// ExitNotInitialized, with initialized:false and G0=NOT_INITIALIZED in the JSON.
func TestDoctorNotInitialized(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	env, out, errb := newEnv("doctor", "--root", root, "--json")
	if code := Run(env); code != ExitNotInitialized {
		t.Fatalf("doctor (uninitialized) = %d, want %d; stderr=%s", code, ExitNotInitialized, errb.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor JSON invalid: %v; got %q", err, out.String())
	}
	if rep.Initialized {
		t.Errorf("report.Initialized = true, want false")
	}
	g0 := findByName(rep.Checks, "config")
	if g0.Code != "NOT_INITIALIZED" {
		t.Errorf("G0 code = %q, want NOT_INITIALIZED", g0.Code)
	}
}

// TestDoctorAllSkip: an initialized but unconfigured root ⇒ every platform probe
// skips, so the command exits 0 without touching the network or a real binary.
func TestDoctorAllSkip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	env, out, errb := newEnv("doctor", "--root", root, "--json")
	if code := Run(env); code != ExitOK {
		t.Fatalf("doctor (unconfigured) = %d, want 0; stderr=%s", code, errb.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor JSON invalid: %v; got %q", err, out.String())
	}
	if !rep.Initialized || !rep.OK {
		t.Errorf("report initialized=%v ok=%v, want both true", rep.Initialized, rep.OK)
	}
	for _, c := range rep.Checks {
		if c.Status == checkFail {
			t.Errorf("check %s failed unexpectedly: %s", c.Name, c.Detail)
		}
	}
}

// findByName returns the first check with the given name (or a zero value).
func findByName(checks []checkResult, name string) checkResult {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return checkResult{}
}
