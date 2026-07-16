package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withSchedulePlatform overrides the schedule command's OS/home/executable
// indirection points for the duration of a test, restoring them afterwards. It
// lets the file-writing path run against a temp HOME on any platform (CI is
// Linux) without touching the real ~/Library/LaunchAgents or launchctl. Tests
// using it must not run in parallel — the overridden vars are package globals.
func withSchedulePlatform(t *testing.T, goos, home, exe string) {
	t.Helper()
	origGOOS, origHome, origExe := scheduleGOOS, userHomeDir, osExecutable
	scheduleGOOS = goos
	userHomeDir = func() (string, error) { return home, nil }
	osExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() {
		scheduleGOOS, userHomeDir, osExecutable = origGOOS, origHome, origExe
	})
}

func schedulePlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", scheduleLabel+".plist")
}

func TestParseScheduleTimes(t *testing.T) {
	ok := []struct {
		in   string
		want []schedTime
	}{
		{"08:00,20:00", []schedTime{{8, 0}, {20, 0}}},
		{"20:00, 08:00", []schedTime{{8, 0}, {20, 0}}},    // sorted
		{"9:5", []schedTime{{9, 5}}},                      // single-digit fields
		{"8", []schedTime{{8, 0}}},                        // bare hour => :00
		{"08:00,08:00,8:00", []schedTime{{8, 0}}},         // de-duplicated
		{"0:00,23:59", []schedTime{{0, 0}, {23, 59}}},     // boundaries
		{"12:30 , 06:15", []schedTime{{6, 15}, {12, 30}}}, // whitespace + sort
	}
	for _, c := range ok {
		got, err := parseScheduleTimes(c.in)
		if err != nil {
			t.Errorf("parseScheduleTimes(%q) error: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseScheduleTimes(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseScheduleTimes(%q)[%d] = %v, want %v", c.in, i, got[i], c.want[i])
			}
		}
	}

	bad := []string{"", "  ", "24:00", "-1:00", "08:60", "08:xx", "noon", ","}
	for _, in := range bad {
		if got, err := parseScheduleTimes(in); err == nil {
			t.Errorf("parseScheduleTimes(%q) = %v, want error", in, got)
		}
	}
}

func TestRenderPlistRoundTrip(t *testing.T) {
	params := plistParams{
		Label:   scheduleLabel,
		BinPath: "/home/a b/.claude/skills/opendoc/bin/opendoc",
		Root:    "/home/a b/.opendoc",
		Times:   []schedTime{{8, 0}, {20, 30}},
		OutLog:  "/home/a b/.opendoc/.internal/logs/launchd.out.log",
		ErrLog:  "/home/a b/.opendoc/.internal/logs/launchd.err.log",
	}
	xml := renderPlist(params)

	// Trigger times round-trip through the parser used by status.
	times := parsePlistTimes(xml)
	if len(times) != 2 || times[0] != (schedTime{8, 0}) || times[1] != (schedTime{20, 30}) {
		t.Errorf("parsePlistTimes round-trip = %v, want [08:00 20:30]", times)
	}
	if bin := parsePlistBinary(xml); bin != params.BinPath {
		t.Errorf("parsePlistBinary = %q, want %q", bin, params.BinPath)
	}
	for _, want := range []string{
		"<key>Label</key>",
		"<string>sync</string>",
		"<key>OPENDOC_ROOT</key>",
		params.Root,
		"<key>RunAtLoad</key>",
		"<false/>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("plist missing %q\n%s", want, xml)
		}
	}
}

func TestRenderPlistEscapesAmpersand(t *testing.T) {
	xml := renderPlist(plistParams{
		Label:   scheduleLabel,
		BinPath: "/home/a&b/opendoc",
		Times:   []schedTime{{8, 0}},
	})
	if strings.Contains(xml, "/home/a&b/opendoc") {
		t.Errorf("raw ampersand leaked into plist:\n%s", xml)
	}
	if !strings.Contains(xml, "/home/a&amp;b/opendoc") {
		t.Errorf("ampersand not escaped:\n%s", xml)
	}
	if got := parsePlistBinary(xml); got != "/home/a&b/opendoc" {
		t.Errorf("parsePlistBinary un-escape = %q, want /home/a&b/opendoc", got)
	}
}

func TestScheduleInstallWritesPlist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "opendoc")
	withSchedulePlatform(t, "darwin", home, bin)

	env, out, errb := newEnv("schedule", "--root", root, "--at", "20:00,08:00")
	if code := Run(env); code != ExitOK {
		t.Fatalf("schedule --at = %d, want %d; stderr=%s", code, ExitOK, errb.String())
	}

	plistPath := schedulePlistPath(home)
	if !fileExists(plistPath) {
		t.Fatalf("plist not written at %s", plistPath)
	}
	report := scheduleShowJSON(t, root, home)
	if !report.Installed {
		t.Fatalf("status after install: not installed")
	}
	if len(report.Times) != 2 || report.Times[0] != "08:00" || report.Times[1] != "20:00" {
		t.Errorf("installed times = %v, want [08:00 20:00] (sorted)", report.Times)
	}
	if report.Binary != bin {
		t.Errorf("installed binary = %q, want %q", report.Binary, bin)
	}

	// The log dir must exist so the unattended job can write its logs.
	if info, err := os.Stat(filepath.Join(root, ".internal", "logs")); err != nil || !info.IsDir() {
		t.Errorf("log dir not created under %s", root)
	}

	// Fresh install prints load + start; it must never run launchctl itself.
	so := out.String()
	for _, want := range []string{"launchctl load", "launchctl start " + scheduleLabel, "sync daily at 08:00, 20:00"} {
		if !strings.Contains(so, want) {
			t.Errorf("install stdout missing %q:\n%s", want, so)
		}
	}
}

func TestScheduleUpdateReloadHint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	home := t.TempDir()
	withSchedulePlatform(t, "darwin", home, filepath.Join(t.TempDir(), "opendoc"))

	env1, _, errb1 := newEnv("schedule", "--root", root, "--at", "08:00,20:00")
	if code := Run(env1); code != ExitOK {
		t.Fatalf("first install = %d; stderr=%s", code, errb1.String())
	}
	env2, out2, errb2 := newEnv("schedule", "--root", root, "--at", "09:00")
	if code := Run(env2); code != ExitOK {
		t.Fatalf("update = %d; stderr=%s", code, errb2.String())
	}
	so := out2.String()
	if !strings.Contains(so, "Updated") {
		t.Errorf("update stdout should say Updated:\n%s", so)
	}
	if !strings.Contains(so, "launchctl unload") || !strings.Contains(so, "launchctl load") {
		t.Errorf("update stdout should show unload+load reload hint:\n%s", so)
	}
	report := scheduleShowJSON(t, root, home)
	if len(report.Times) != 1 || report.Times[0] != "09:00" {
		t.Errorf("times after update = %v, want [09:00]", report.Times)
	}
}

func TestScheduleRemove(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	home := t.TempDir()
	withSchedulePlatform(t, "darwin", home, filepath.Join(t.TempDir(), "opendoc"))

	install, _, errb := newEnv("schedule", "--root", root, "--at", "08:00")
	if code := Run(install); code != ExitOK {
		t.Fatalf("install = %d; stderr=%s", code, errb.String())
	}

	// `opendoc unschedule` is the alias for `opendoc schedule --remove`.
	rm, out, errb := newEnv("unschedule", "--root", root)
	if code := Run(rm); code != ExitOK {
		t.Fatalf("unschedule = %d; stderr=%s", code, errb.String())
	}
	if fileExists(schedulePlistPath(home)) {
		t.Errorf("plist still present after unschedule")
	}
	if !strings.Contains(out.String(), "launchctl unload") {
		t.Errorf("remove stdout should show unload hint:\n%s", out.String())
	}

	// Removing again is an idempotent no-op success.
	rm2, out2, _ := newEnv("schedule", "--root", root, "--remove")
	if code := Run(rm2); code != ExitOK {
		t.Fatalf("second remove = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out2.String(), "nothing to remove") {
		t.Errorf("second remove stdout = %q, want 'nothing to remove'", out2.String())
	}
}

func TestScheduleStatusNotInstalled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	home := t.TempDir()
	withSchedulePlatform(t, "darwin", home, filepath.Join(t.TempDir(), "opendoc"))

	env, out, errb := newEnv("schedule", "--root", root)
	if code := Run(env); code != ExitOK {
		t.Fatalf("status = %d; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("status stdout = %q, want 'not installed'", out.String())
	}
}

func TestScheduleNonDarwin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	withSchedulePlatform(t, "linux", t.TempDir(), filepath.Join(t.TempDir(), "opendoc"))

	env, _, errb := newEnv("schedule", "--root", root, "--at", "08:00")
	if code := Run(env); code != ExitError {
		t.Fatalf("schedule on linux = %d, want %d", code, ExitError)
	}
	if !strings.Contains(errb.String(), "macOS") {
		t.Errorf("stderr should explain macOS-only: %q", errb.String())
	}
}

func TestScheduleNotInitialized(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror") // no config written
	withSchedulePlatform(t, "darwin", t.TempDir(), filepath.Join(t.TempDir(), "opendoc"))

	env, _, errb := newEnv("schedule", "--root", root, "--at", "08:00")
	if code := Run(env); code != ExitNotInitialized {
		t.Fatalf("schedule (uninitialized) = %d, want %d", code, ExitNotInitialized)
	}
	if !strings.Contains(errb.String(), "setup.md") {
		t.Errorf("stderr missing setup.md pointer: %q", errb.String())
	}
}

func TestScheduleMutuallyExclusiveFlags(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	withSchedulePlatform(t, "darwin", t.TempDir(), filepath.Join(t.TempDir(), "opendoc"))

	env, _, errb := newEnv("schedule", "--root", root, "--at", "08:00", "--remove")
	if code := Run(env); code != ExitUsage {
		t.Fatalf("--at with --remove = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("stderr should explain the conflict: %q", errb.String())
	}
}

// scheduleShowJSON runs `opendoc schedule --json` and decodes the status payload.
func scheduleShowJSON(t *testing.T, root, home string) scheduleStatusReport {
	t.Helper()
	env, out, errb := newEnv("schedule", "--root", root, "--json")
	if code := Run(env); code != ExitOK {
		t.Fatalf("schedule --json = %d; stderr=%s", code, errb.String())
	}
	var report scheduleStatusReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("status not valid JSON: %v; got %q", err, out.String())
	}
	return report
}
