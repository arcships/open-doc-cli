package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/arcships/open-doc-cli/internal/layout"
)

// scheduleLabel is the launchd job label for the unattended sync. It is stable
// across installs so a re-schedule replaces the same job, and it names the plist
// template (com.arcships.opendoc.sync.plist).
const scheduleLabel = "com.arcships.opendoc.sync"

// Indirection points for testing: the real os functions by default, overridable
// in schedule_test.go so the command's file-writing path can be exercised
// against a temp HOME on any platform without touching the real LaunchAgents dir
// or shelling out to launchctl.
var (
	osExecutable = os.Executable
	userHomeDir  = os.UserHomeDir
	scheduleGOOS = runtime.GOOS
)

// schedTime is one daily trigger point (local time), the unit of a launchd
// StartCalendarInterval entry.
type schedTime struct {
	Hour   int
	Minute int
}

func (t schedTime) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// scheduleStatusReport is the machine-friendly `opendoc schedule` (status mode)
// payload: whether the LaunchAgent is installed and, if so, its trigger times
// and the binary it invokes. Read-only — status mode never writes.
type scheduleStatusReport struct {
	Installed bool     `json:"installed"`
	PlistPath string   `json:"plist_path"`
	Times     []string `json:"times,omitempty"`
	Binary    string   `json:"binary,omitempty"`
}

// runSchedule implements `opendoc schedule`: manage the twice-(or N-times-)daily
// unattended `opendoc sync` launchd job. With --at it writes/updates the LaunchAgent
// plist (correct absolute paths, chosen times); with --remove it deletes it;
// with neither it prints a read-only status. It never runs launchctl itself —
// loading a job changes user/system state and its first run must happen with a
// human present to clear macOS approval prompts, so the command prints the exact
// launchctl lines for the user to run.
func runSchedule(env Env, args []string) int {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "mirror root (overrides OPENDOC_ROOT and ~/.opendoc)")
	at := fs.String("at", "", "comma-separated daily times HH:MM to run sync (e.g. 08:00,20:00)")
	remove := fs.Bool("remove", false, "remove the scheduled sync job")
	asJSON := fs.Bool("json", false, "emit status as JSON (status mode only)")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: opendoc schedule [--at HH:MM,HH:MM | --remove] [flags]\n\n"+
			"Install, inspect, or remove the unattended `opendoc sync` launchd job (macOS).\n\n"+
			"  opendoc schedule                     show the current schedule (read-only)\n"+
			"  opendoc schedule --at 08:00,20:00    install/update: run sync at those times\n"+
			"  opendoc schedule --remove            remove the scheduled job\n\n"+
			"opendoc writes the LaunchAgent plist but never runs launchctl — it prints the\n"+
			"launchctl lines for you to run, so the first run happens with you present\n"+
			"to clear any macOS approval prompts.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(env.Stderr, "opendoc schedule: too many arguments\n")
		return ExitUsage
	}
	if *remove && *at != "" {
		fmt.Fprintf(env.Stderr, "opendoc schedule: --at and --remove are mutually exclusive\n")
		return ExitUsage
	}

	if scheduleGOOS != "darwin" {
		fmt.Fprintf(env.Stderr, "opendoc schedule: unattended scheduling uses launchd and is only supported on macOS\n")
		return ExitError
	}

	l, err := layout.Resolve(*root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: %v\n", err)
		return ExitError
	}
	if code := requireInitialized(env, l, "schedule"); code != -1 {
		return code
	}

	home, err := userHomeDir()
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: resolve home dir: %v\n", err)
		return ExitError
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", scheduleLabel+".plist")

	switch {
	case *remove:
		return scheduleRemove(env, plistPath)
	case *at != "":
		return scheduleInstall(env, l, plistPath, *at)
	default:
		return scheduleShow(env, plistPath, *asJSON)
	}
}

// scheduleInstall renders the LaunchAgent plist for the chosen times and writes
// it into ~/Library/LaunchAgents, creating the LaunchAgents and log directories
// as needed. It prints the launchctl commands the user must run to (re)load it.
func scheduleInstall(env Env, l layout.Layout, plistPath, atSpec string) int {
	times, err := parseScheduleTimes(atSpec)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: %v\n", err)
		return ExitUsage
	}

	bin, err := scheduleBinaryPath()
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: locate opendoc binary: %v\n", err)
		return ExitError
	}

	// The scheduled job must be able to write its logs; create the dir now rather
	// than depend on a prior sync having made it.
	if err := os.MkdirAll(l.LogsDir(), 0o755); err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: create log dir: %v\n", err)
		return ExitError
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: create LaunchAgents dir: %v\n", err)
		return ExitError
	}

	existed := fileExists(plistPath)

	content := renderPlist(plistParams{
		Label:   scheduleLabel,
		BinPath: bin,
		Root:    l.Root,
		Times:   times,
		OutLog:  filepath.Join(l.LogsDir(), "launchd.out.log"),
		ErrLog:  filepath.Join(l.LogsDir(), "launchd.err.log"),
	})
	if err := writeFileAtomic(plistPath, []byte(content), 0o644); err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: write plist: %v\n", err)
		return ExitError
	}

	verb := "Wrote"
	if existed {
		verb = "Updated"
	}
	fmt.Fprintf(env.Stdout, "%s LaunchAgent: %s\n", verb, plistPath)
	fmt.Fprintf(env.Stdout, "Schedule: sync daily at %s\n", joinTimes(times))
	fmt.Fprintf(env.Stdout, "Binary:   %s\n", bin)
	fmt.Fprintf(env.Stdout, "\nRun these yourself to activate it — opendoc does not touch launchctl:\n")
	if existed {
		fmt.Fprintf(env.Stdout, "  launchctl unload %s\n", plistPath)
		fmt.Fprintf(env.Stdout, "  launchctl load %s\n", plistPath)
	} else {
		fmt.Fprintf(env.Stdout, "  launchctl load %s\n", plistPath)
		fmt.Fprintf(env.Stdout, "  launchctl start %s   # run once now, at the screen, to clear any macOS prompts\n", scheduleLabel)
	}
	fmt.Fprintf(env.Stdout, "\nUnattended runs read tokens from %s (NOTION_TOKEN, OPENDOC_LARK_CLI).\n", l.EnvFilePath())
	fmt.Fprintf(env.Stdout, "See references/launchd/README.md for the env file and first-run approval details.\n")
	return ExitOK
}

// scheduleRemove deletes the LaunchAgent plist. It is idempotent: removing an
// absent job is a no-op success. It prints the launchctl line to stop a job that
// may still be loaded.
func scheduleRemove(env Env, plistPath string) int {
	if !fileExists(plistPath) {
		fmt.Fprintf(env.Stdout, "No scheduled sync job at %s (nothing to remove).\n", plistPath)
		return ExitOK
	}
	if err := os.Remove(plistPath); err != nil {
		fmt.Fprintf(env.Stderr, "opendoc schedule: remove plist: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "Removed LaunchAgent: %s\n", plistPath)
	fmt.Fprintf(env.Stdout, "\nIf it is currently loaded, stop it yourself:\n")
	fmt.Fprintf(env.Stdout, "  launchctl unload %s\n", plistPath)
	return ExitOK
}

// scheduleShow prints the current schedule (read-only). It reports "not
// installed" when no plist exists, otherwise the parsed trigger times and the
// binary the job invokes.
func scheduleShow(env Env, plistPath string, asJSON bool) int {
	report := scheduleStatusReport{PlistPath: plistPath}
	if fileExists(plistPath) {
		data, err := os.ReadFile(plistPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "opendoc schedule: read plist: %v\n", err)
			return ExitError
		}
		report.Installed = true
		for _, t := range parsePlistTimes(string(data)) {
			report.Times = append(report.Times, t.String())
		}
		report.Binary = parsePlistBinary(string(data))
	}

	if asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(env.Stderr, "opendoc schedule: %v\n", err)
			return ExitError
		}
		return ExitOK
	}

	if !report.Installed {
		fmt.Fprintf(env.Stdout, "schedule: not installed (%s)\n", plistPath)
		fmt.Fprintf(env.Stdout, "install with: opendoc schedule --at 08:00,20:00\n")
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "schedule: installed (%s)\n", plistPath)
	if len(report.Times) > 0 {
		fmt.Fprintf(env.Stdout, "times:    %s\n", strings.Join(report.Times, ", "))
	} else {
		fmt.Fprintf(env.Stdout, "times:    (unparsed — plist not in opendoc's format)\n")
	}
	if report.Binary != "" {
		fmt.Fprintf(env.Stdout, "binary:   %s\n", report.Binary)
	}
	return ExitOK
}

// parseScheduleTimes parses a comma-separated "HH:MM" (or bare "HH", minute 0)
// list into sorted, de-duplicated trigger points. Hours are 0-23, minutes 0-59.
func parseScheduleTimes(s string) ([]schedTime, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("--at: no times given")
	}
	var out []schedTime
	seen := map[schedTime]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		hStr, mStr := part, "0"
		if i := strings.IndexByte(part, ':'); i >= 0 {
			hStr, mStr = part[:i], part[i+1:]
		}
		h, err := strconv.Atoi(strings.TrimSpace(hStr))
		if err != nil || h < 0 || h > 23 {
			return nil, fmt.Errorf("--at: invalid time %q (hour must be 00-23)", part)
		}
		m, err := strconv.Atoi(strings.TrimSpace(mStr))
		if err != nil || m < 0 || m > 59 {
			return nil, fmt.Errorf("--at: invalid time %q (minute must be 00-59)", part)
		}
		t := schedTime{Hour: h, Minute: m}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--at: no valid times given")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hour != out[j].Hour {
			return out[i].Hour < out[j].Hour
		}
		return out[i].Minute < out[j].Minute
	})
	return out, nil
}

// plistParams are the fields renderPlist substitutes into the LaunchAgent
// template. All path values are absolute (launchd does not expand ~/$HOME).
type plistParams struct {
	Label   string
	BinPath string
	Root    string
	Times   []schedTime
	OutLog  string
	ErrLog  string
}

// renderPlist produces the LaunchAgent plist text. It sets OPENDOC_ROOT explicitly so
// the job locates the same mirror root (and its .internal/env token file) the
// command was run against, and points StandardOut/ErrorPath at the mirror's log
// dir. RunAtLoad is false: this is a scheduled job, not a daemon.
func renderPlist(p plistParams) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- Generated by `opendoc schedule`. Re-run opendoc schedule to change the times;\n")
	b.WriteString("     do not hand-edit paths (they must stay absolute). -->\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	fmt.Fprintf(&b, "    <key>Label</key>\n    <string>%s</string>\n\n", xmlEscape(p.Label))

	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(p.BinPath))
	b.WriteString("        <string>sync</string>\n    </array>\n\n")

	b.WriteString("    <key>StartCalendarInterval</key>\n    <array>\n")
	for _, t := range p.Times {
		b.WriteString("        <dict>\n")
		fmt.Fprintf(&b, "            <key>Hour</key><integer>%d</integer>\n", t.Hour)
		fmt.Fprintf(&b, "            <key>Minute</key><integer>%d</integer>\n", t.Minute)
		b.WriteString("        </dict>\n")
	}
	b.WriteString("    </array>\n\n")

	if p.Root != "" {
		b.WriteString("    <key>EnvironmentVariables</key>\n    <dict>\n")
		fmt.Fprintf(&b, "        <key>OPENDOC_ROOT</key>\n        <string>%s</string>\n", xmlEscape(p.Root))
		b.WriteString("    </dict>\n\n")
	}

	fmt.Fprintf(&b, "    <key>StandardOutPath</key>\n    <string>%s</string>\n", xmlEscape(p.OutLog))
	fmt.Fprintf(&b, "    <key>StandardErrorPath</key>\n    <string>%s</string>\n", xmlEscape(p.ErrLog))

	b.WriteString("\n    <key>RunAtLoad</key>\n    <false/>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// plistTimeRe matches a StartCalendarInterval Hour/Minute pair in the format
// renderPlist emits, so scheduleShow can echo back the installed times.
var plistTimeRe = regexp.MustCompile(`<key>Hour</key>\s*<integer>(\d+)</integer>\s*<key>Minute</key>\s*<integer>(\d+)</integer>`)

// plistBinaryRe captures the first ProgramArguments string (the opendoc binary path).
var plistBinaryRe = regexp.MustCompile(`<key>ProgramArguments</key>\s*<array>\s*<string>([^<]*)</string>`)

// parsePlistTimes extracts the trigger times from an opendoc-generated plist. Times in
// a foreign-formatted plist may not parse; callers treat an empty result as
// "unknown", not an error.
func parsePlistTimes(s string) []schedTime {
	var out []schedTime
	for _, m := range plistTimeRe.FindAllStringSubmatch(s, -1) {
		h, err1 := strconv.Atoi(m[1])
		mm, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, schedTime{Hour: h, Minute: mm})
	}
	return out
}

// parsePlistBinary extracts the invoked binary path from a plist, or "" if absent.
func parsePlistBinary(s string) string {
	if m := plistBinaryRe.FindStringSubmatch(s); m != nil {
		return xmlUnescape(m[1])
	}
	return ""
}

// scheduleBinaryPath returns the absolute path to the running opendoc binary, with
// symlinks resolved so the plist points at the real file. It falls back to the
// unresolved path if EvalSymlinks fails.
func scheduleBinaryPath() (string, error) {
	exe, err := osExecutable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// joinTimes renders times as "08:00, 20:00".
func joinTimes(ts []schedTime) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.String()
	}
	return strings.Join(parts, ", ")
}

// writeFileAtomic writes data to path via a temp file + rename, so a crash leaves
// either the old file or the fully written new one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opendoc-plist-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// xmlEscaper/xmlUnescaper cover the character set that appears in filesystem
// paths inside plist <string> values (& < > " '). encoding/xml is overkill for
// this fixed, generated template.
var (
	xmlEscaper   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	xmlUnescaper = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'")
)

func xmlEscape(s string) string   { return xmlEscaper.Replace(s) }
func xmlUnescape(s string) string { return xmlUnescaper.Replace(s) }
