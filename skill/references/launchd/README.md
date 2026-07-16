# Unattended: launchd runs sync on a schedule

Have launchd run `opendoc sync` automatically each day so the daytime mirror is never
more than a few hours stale. The default cadence is twice a day
(08:00 / 20:00), but you choose the times.

## The easy way: `opendoc schedule`

`opendoc schedule` writes the LaunchAgent plist for you — correct absolute paths
(launchd does **not** expand `~`/`$HOME`), your chosen times, `OPENDOC_ROOT` and log
routing all filled in. No `sed`, no hand-editing XML.

```sh
opendoc schedule --at 08:00,20:00     # install / update: run sync at those times
opendoc schedule                      # show the current schedule (read-only)
opendoc schedule --remove             # remove it   (alias: opendoc unschedule)
```

`--at` takes a comma-separated `HH:MM` list (a bare `8` means `08:00`); times are
validated, de-duplicated, and sorted. Re-run `--at` any time to change the schedule.

**`opendoc schedule` writes the plist but never runs `launchctl`** — loading a job
changes user state and its first run must happen with a human present (see below). The
command prints the exact `launchctl` lines for **you** to run:

```sh
launchctl load ~/Library/LaunchAgents/com.arcships.opendoc.sync.plist
launchctl start com.arcships.opendoc.sync   # run once now, at the screen (see "First run")
```

Changing the times later prints the reload pair instead (`launchctl unload` then
`launchctl load`) — a loaded job must be reloaded to pick up an edited plist.

### First run — MANDATORY, not just a verification nicety

After `launchctl load`, immediately run it once **while you are at the screen**:

```sh
launchctl start com.arcships.opendoc.sync
```

then check `<root>/.internal/logs/launchd.out.log` / `launchd.err.log`. The real
purpose: **flush every macOS approval while a human can click it.** The first
launchd-context run is when macOS may prompt — the "Background Items Added" / Login
Items approval, a Keychain dialog (click **Always Allow**, or the 08:00 run re-prompts
into a void), or a folder-access (TCC) dialog. A prompt that first appears at an
unattended trigger hangs or silently fails the job with nobody there to click it. The
install is not done until this manual start completes with a clean sync report in the log.

## Token / PATH injection for unattended runs (native env file — no wrapper)

launchd does **not** read shell profiles (`~/.zshrc` etc.), so an `export NOTION_TOKEN`
there has no effect on unattended runs. **opendoc reads the token from a private env
file natively**:

```
<root>/.internal/env        # default ~/.opendoc/.internal/env
```

containing shell-style `export KEY="value"` / `KEY=value` lines (opendoc parses them
directly, no shell evaluation). When `NOTION_TOKEN` is not already in the process
environment, opendoc falls back to this file before it builds the Notion adapter. **The
token lives only in this 0600 file, never in the plist.**

```sh
mkdir -p ~/.opendoc/.internal
printf 'export NOTION_TOKEN="%s"\n' 'ntn_your_token' > ~/.opendoc/.internal/env
chmod 600 ~/.opendoc/.internal/env
```

The Feishu engine needs no PATH handling at all: it is embedded in the opendoc binary
itself (`opendoc lark-engine ...`), so launchd's bare PATH cannot lose it. Feishu-only
users need no env file. (Debugging escape hatch: setting `OPENDOC_LARK_CLI` — in the
environment or this file — points opendoc at an external lark-cli binary instead of
the embedded engine; leave it unset in normal operation.)

> **`offline_access` must be in the Feishu OAuth grant** (see `../setup.md` F2/F3) for
> unattended runs — without the refresh token the Feishu access token expires and
> scheduled syncs start failing mid-day.

## What if the Mac is asleep (or off) at a trigger time?

- **Sleep needs no handling.** `StartCalendarInterval` differs from cron: trigger
  points missed during sleep run **once on wake**, and multiple missed points (e.g.
  lid closed over a weekend) are **coalesced into a single run** — documented
  `launchd.plist(5)` behavior. With the everyday "close lid at night, open in the
  morning" pattern, the 08:00 run simply fires when the lid opens.
- **A full shutdown across a trigger point is not made up** (launchd has no
  anacron-style catch-up at boot); the next scheduled run covers it — incremental
  sync catches up idempotently, so a gap only means "stale", never "lost".
- The second safety net is SKILL.md's session discipline: before retrieving, the
  agent compares the last-sync time in the `INDEX.md` header and can run `opendoc sync`
  on the spot if the mirror is clearly stale.

## The manual template (fallback)

`com.arcships.opendoc.sync.plist` in this directory is the same plist `opendoc schedule`
generates, kept as a reference. If you cannot run `opendoc schedule` (e.g. building the
job by hand or on a machine without the built binary), copy it, replace every `__HOME__`
with your absolute home directory, adjust the `StartCalendarInterval` times, write it to
`~/Library/LaunchAgents/`, then follow the same `launchctl load` + first-run steps above.
Non-default mirror root: set `OPENDOC_ROOT` in the plist's `EnvironmentVariables` block
(opendoc reads it to locate both the data dir and the `<root>/.internal/env` token file).

## Notes

- **opendoc never runs `launchctl` and never changes system settings** — with
  `opendoc schedule` or by hand, the `launchctl load` / `unload` / `start` steps are
  always yours to run.
- Resumability naturally covers "quota wall / mid-run failure": whatever a round
  missed, the next round picks up — nothing extra to handle.
