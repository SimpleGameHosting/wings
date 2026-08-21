# SGH Wings Patches

This fork tracks upstream `pterodactyl/wings` with a short, linear patch series.
Base: upstream develop @ ae6c629 (pinned; carries dependency security updates missing from the last upstream release tag; functional drift from v1.11.13 reviewed and accepted 2026-08-21 - stop/kill redesign, MB->MiB memory limits, hard-link dedup, symlink fix, /etc/passwd mount - the canary validates these alongside the crash callback).
Deployable builds are tagged `v1.11.13-sgh.<n>`.

Every SGH modification MUST be registered here before its work is considered complete.

## Rebase runbook (on upstream activity)

1. `git fetch upstream`
2. Pick the new base: the newest upstream release tag, or a pinned `develop` commit when tags lag on security updates.
3. `git rebase --onto <new-base> <old-base> sgh`
4. Resolve conflicts patch by patch; each entry below lists its touched files.
5. `go build ./... && go test -race ./...`
6. Update the Base line above, tag `v<upstream>-sgh.<n+1>`, push with `--force-with-lease`.
7. Canary one node, then roll the fleet via `playbooks/pterodactyl/wings_update`.

## Patches

### remote: ReportCrash panel callback

- What: adds `ReportCrash` to the remote client (interface + implementation) with `CrashReportRequest`.
- Why: the panel's crash analyzer needs authoritative crash detection (spec: panel repo, docs/superpowers/specs/2026-08-20-crash-analyzer-design.md).
- Files: `remote/http.go`, `remote/types.go`, `remote/servers.go`, `remote/servers_test.go`.
- Conflict risk on rebase: low; appends only.

### server: report crashes to the panel

- What: `handleServerCrash()` fires `ReportCrash` in a fire-and-forget goroutine (30s timeout) before the restart-throttle check; `CrashHandler` gains a mutex-guarded `lastUptime` stashed in `OnStateChange` before stats reset.
- Why: authoritative crash detection for the panel crash analyzer, including unattended and throttled crashes; the stash exists because `s.resources.Reset()` zeroes uptime before the crash handler runs.
- Files: `server/crash.go`, `server/server.go`, `server/crash_test.go`.
- Conflict risk on rebase: low-medium; touches two lines inside `OnStateChange`.

### ci: sgh branch triggers, upstream watch, govulncheck

- What: push.yaml/release.yaml retargeted for the fork (sgh branch, generated release notes, no upstream release-branch automation); adds scheduled upstream-activity issues and weekly govulncheck.
- Why: Kane's fork constraints - active upstream security tracking without waiting on a maintenance-mode upstream.
- Files: `.github/workflows/*.yaml`.
- Conflict risk on rebase: low; upstream rarely touches workflows beyond dependabot bumps.

### deps: security updates (govulncheck)

- What: toolchain go1.24.1 -> go1.25.14 (go.mod's `go` line was auto-raised 1.23.0 -> 1.25.0 by `go mod tidy`, which the new dependency graph requires); golang.org/x/crypto v0.41.0 -> v0.52.0; golang.org/x/net v0.42.0 -> v0.55.0; golang.org/x/text v0.28.0 -> v0.39.0; github.com/ulikunitz/xz v0.5.14 -> v0.5.15. `go mod tidy` also carried golang.org/x/sync, golang.org/x/sys and golang.org/x/term forward transitively. Also fixes a `cmd/configure.go` non-constant-format-string vet finding that the go-line bump exposed: `fmt.Printf(req.URL.String())` -> `fmt.Print(req.URL.String())`, identical output, no format-string risk.
- Why: the govulncheck workflow found 53 vulnerabilities affecting this code (39 stdlib from go1.24.1, 14 across the five modules above). Verified with govulncheck itself (vuln DB dated 2026-08-19) that none of the 39 stdlib findings are fixed by any go1.24.x release: go1.24.13 is the last 1.24.x patch ever cut, and every "Fixed in" for those 39 points at go1.25.8 through go1.25.13, i.e. Go 1.24 has left its security-support window. Moved to go1.25.14 (newest 1.25.x patch, confirmed against `golang:1.25` and go.dev/dl) instead of the go1.24.9+ originally assumed. govulncheck now reports 2 remaining findings, both documented below as BLOCKED, down from 53.
- Not fixed (BLOCKED): github.com/docker/docker v28.3.3+incompatible still trips GO-2026-4887 and GO-2026-4883 (Moby AuthZ plugin bypass; plugin-privilege off-by-one). Both advisories carry no `fixed` version anywhere under the `github.com/docker/docker` module path, only under the renamed `github.com/moby/moby/v2` path (>= v2.0.0-beta.8, a pre-1.0 major rewrite). Wings imports `github.com/docker/docker` directly in 13 files (cmd/diagnostics.go, cmd/root.go, config/config_docker.go, server/backup.go, server/install.go, system/system.go, environment/docker/environment.go, environment/settings.go, environment/docker.go, environment/docker/stats.go, environment/docker/power.go, environment/docker/container.go, environment/docker/api.go). Migrating the import path (and likely client-API call sites) to a beta major-version rewrite is out of scope for a dependency patch; needs its own ruling and task.
- Files: `go.mod`, `go.sum`, `cmd/configure.go`, `.github/workflows/push.yaml`, `.github/workflows/release.yaml`, `.github/workflows/govulncheck.yaml`.
- Conflict risk on rebase: low for the module bumps and workflow version strings (go.sum regenerates cleanly via `go mod tidy`, and only version-string lines changed in the workflows); low for `cmd/configure.go` (one line). Re-run govulncheck after every rebase since upstream Go and the five bumped modules both ship security patches on their own cadence.
