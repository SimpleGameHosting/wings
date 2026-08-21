# SGH Wings Patches

This fork tracks upstream `pterodactyl/wings` with a short, linear patch series.
Base: upstream develop @ ae6c629 (pinned; carries dependency security updates missing from the last upstream release tag; functional drift from v1.11.13 reviewed and accepted 2026-08-21 as a whole - stop/kill redesign, MB->MiB memory limits, hard-link dedup, symlink fix, /etc/passwd mount, and a rewritten internal/ufs filesystem layer (~975/-249) - the canary validates these alongside the crash callback).
Deployable builds are tagged `v1.11.13-sgh.<n>`.

Every SGH modification MUST be registered here before its work is considered complete.

## Rebase runbook (on upstream activity)

1. `git fetch upstream`
2. Pick the new base: the newest upstream release tag, or a pinned `develop` commit when tags lag on security updates.
3. `git rebase --onto <new-base> <old-base> sgh`
4. Resolve conflicts patch by patch; each entry below lists its touched files.
5. `gofmt -l . && go vet ./... && CGO_ENABLED=1 go test -race ./... && CGO_ENABLED=0 go build ./...` - run on Linux, or inside a golang container (Wings does not build natively on macOS). `-race` needs cgo, so the test step cannot run with `CGO_ENABLED=0`; the build step uses `CGO_ENABLED=0` to match the binaries push.yaml and release.yaml publish.
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
- Why: the govulncheck workflow found 53 vulnerabilities affecting this code (39 stdlib from go1.24.1, 14 across the five modules above). Verified with govulncheck itself (vuln DB dated 2026-08-19) that none of the 39 stdlib findings are fixed by any go1.24.x release: go1.24.13 is the last 1.24.x patch ever cut, and every "Fixed in" for those 39 points at go1.25.8 through go1.25.13, i.e. Go 1.24 has left its security-support window. go1.25.13 is the minimal toolchain that clears every stdlib finding; go1.25.14 (the newest 1.25.x patch, confirmed against `golang:1.25` and go.dev/dl) was chosen deliberately instead of stopping at that minimum, and instead of the go1.24.9+ originally assumed. govulncheck now reports 2 remaining findings, both documented below as BLOCKED, down from 53.
- Not fixed (BLOCKED): github.com/docker/docker v28.3.3+incompatible still trips GO-2026-4887 and GO-2026-4883 (Moby AuthZ plugin bypass; plugin-privilege off-by-one). Both advisories carry no `fixed` version anywhere under the `github.com/docker/docker` module path, only under the renamed `github.com/moby/moby/v2` path (>= v2.0.0-beta.8, a pre-1.0 major rewrite). Wings imports `github.com/docker/docker` directly in 13 files (cmd/diagnostics.go, cmd/root.go, config/config_docker.go, server/backup.go, server/install.go, system/system.go, environment/docker/environment.go, environment/settings.go, environment/docker.go, environment/docker/stats.go, environment/docker/power.go, environment/docker/container.go, environment/docker/api.go). Migrating the import path (and likely client-API call sites) to a beta major-version rewrite is out of scope for a dependency patch; needs its own ruling and task.
- Files: `go.mod`, `go.sum`, `cmd/configure.go`, `.github/workflows/push.yaml`, `.github/workflows/release.yaml`, `.github/workflows/govulncheck.yaml`.
- Conflict risk on rebase: low for the module bumps and workflow version strings (go.sum regenerates cleanly via `go mod tidy`, and only version-string lines changed in the workflows); low for `cmd/configure.go` (one line). Re-run govulncheck after every rebase since upstream Go and the five bumped modules both ship security patches on their own cadence.

### ci: hardening

- What: upstream-watch.yaml gains a `concurrency` group (`upstream-watch`, cancel-in-progress false) so overlapping scheduled/dispatched runs cannot file duplicate issues, and its BASE extraction now runs under `set -euo pipefail` with an explicit empty-check that fails the job instead of silently falling back to a `HEAD..upstream/develop` comparison (an empty `BASE` widened the double-dot range to the runner's own `HEAD`, undercounting or misreporting upstream drift). govulncheck.yaml gains a least-privilege `permissions: contents: read` block and now runs `govulncheck -json ./...` through a small shell/jq filter instead of the bare text-mode command: the filter keeps only findings whose trace actually reaches a called function (`select(has("function"))` on a trace frame, distinguishing "affected" from "imported but not called"), reduces those to unique OSV IDs, and subtracts the IDs listed in the new committed `.govulncheck-exceptions` file; the job fails only if an unlisted ID remains, printing both the accepted and unexpected sets either way. push.yaml's two artifact-upload steps were still gated on `github.ref == 'refs/heads/develop'` from the prior retarget pass, so sgh pushes silently skipped artifact uploads; both gates now check `refs/heads/sgh`.
- Why: the concurrency and fail-fast fixes close gaps found while hardening upstream-watch.yaml for unattended runs; the govulncheck exceptions filter turns the two BLOCKED docker/docker findings above into an enforced, self-documenting allowlist instead of a workflow that either silently ignores all findings or blocks on findings that were already ruled out; the push.yaml fix restores artifact uploads on the branch this fork actually pushes to.
- Files: `.github/workflows/upstream-watch.yaml`, `.github/workflows/govulncheck.yaml`, `.govulncheck-exceptions`, `.github/workflows/push.yaml`.
- Conflict risk on rebase: low; upstream rarely touches these workflow files beyond dependabot version bumps, and `.govulncheck-exceptions` is fork-only.

### vet: drop unreachable returns

- What: deletes the `return` statements that follow `panic(...)` in the `NewFs()` test helper (`server/filesystem/filesystem_test.go`, three sites) and in `router.Configure` (`router/router.go`, one site). `panic` never returns, so none of the four lines could execute; `go vet ./...` reported each as "unreachable code". No behavior change.
- Why: `go vet ./...` is part of the fork's verification gate (alongside `gofmt -l`, `go test -race` and `go build`) and has to pass cleanly for that gate to mean anything. All four findings are pure upstream code (d1c0ca52 and ff50d0e5) that upstream has not fixed as of `upstream/develop` @ d611682.
- Files: `server/filesystem/filesystem_test.go`, `router/router.go`.
- Conflict risk on rebase: low; four deleted lines, and upstream's later commits to both files (token masking in the request logger, new filesystem tests) do not touch them. If upstream ever deletes the same lines itself, this patch becomes empty and can be dropped from the series.

### vet: stop copying mutexes (ResourceUsage, Configuration, Download)

- What: splits the lock out of the three types `go vet` flagged for copying a `sync.RWMutex` by value, with every public signature and JSON payload kept identical. `ResourceUsage` is now a plain snapshot value (embedded `environment.Stats`, `State`, `Disk`); the lock moved to a new unexported `resourceTracker{mu; ResourceUsage}` held by `Server.resources`, `UpdateStats`/`Reset` moved onto it, and `Proc()` returns `s.resources.ResourceUsage` under the lock. Embedding keeps every `s.resources.X` call site untouched. `Configuration` keeps its name and methods, but its settings fields moved verbatim into a new exported `ConfigurationData` that it embeds; `SyncWithConfiguration` decodes into `ConfigurationData` and assigns `s.cfg.ConfigurationData = c` under the existing lock, deleting upstream's "lock the new struct before copying it over the old one" dance; `APIResponse.Configuration` becomes `ConfigurationData`, filled from a new read-locked `configurationSnapshot()` instead of `*s.Config()` (a fourth whole-struct copy that vet's call-expression heuristic does not report). `Download.MarshalJSON` takes a pointer receiver; every marshal path already goes through `*Download` (`ByServer` returns `[]*Download`). The three `//goland:noinspection GoVetCopyLock` suppressions went with the code they suppressed. New tests in `server/resources_test.go`, `server/configuration_test.go`, `server/server_test.go` and `router/downloader/downloader_test.go` cover snapshot independence, wholesale-replace sync semantics (including the node-level crash detection default), a stranded-waiter hammer test, concurrent marshal-versus-progress writes, and JSON goldens for the stats, configuration and full `ToAPIResponse` payloads captured from the pre-patch code.
- Why: the findings were real bugs, not style. The value-receiver `MarshalJSON` copied `Download` (mutex and `progress` included) without the lock while the download goroutine writes it; on the pre-patch code the new test reports the data race and then hangs for the full test timeout, because the copy can capture a locked mutex that `Progress()` waits on forever - so `GET /api/servers/:server/files/pull` could hang during an active download. `s.cfg = c` overwrote the live mutex's reader/writer counters with those of the freshly locked temporary, stranding any goroutine already queued on `s.cfg.mu` (every `Config()`, `DiskSpace()` and `SetSuspended()` caller) forever; the new hammer test hits that hang within 2000 syncs on the pre-patch code and finishes in about 0.1s afterwards. `Proc()` returned a copy of a locked mutex inside every stats snapshot published to websockets and the API, harmless only as long as nobody ever locks the copy. Upstream has fixed none of these as of `upstream/develop` @ d611682.
- Files: `server/resources.go`, `server/configuration.go`, `server/server.go`, `router/downloader/downloader.go`, `server/resources_test.go`, `server/configuration_test.go`, `server/server_test.go`, `router/downloader/downloader_test.go`.
- Conflict risk on rebase: low-medium. `server/resources.go` has had no upstream commits since 2022. The `Configuration` field block is unchanged line for line (only the type header above it and the wrapper below it are new), so upstream field additions apply cleanly. `server/server.go` changes the `resources` field line, the `SyncWithConfiguration` body and `APIResponse`/`ToAPIResponse`, none of which upstream's later commits (suspension disconnects in `Sync()`, machine-id in `CreateEnvironment()`) touch; `OnStateChange`, where the crash-handler patch lives, still calls `s.Proc().Uptime` and `s.resources.Reset()` unchanged. `downloader.go` changes one signature that upstream's later `IsDownloadError` commit does not touch. The four test files are fork-only.

### server: content fingerprint endpoint

- What: adds `Filesystem.Fingerprint(ctx, ignore)` which walks the server root with the archiver's gitignore matcher and digests `(relative path, size, mtime)` of included files, the path of included directories, and `(relative path, lstat mtime)` of included symlinks so that retargeting a link is detected; each entry is reduced to its own SHA-256 digest as it is visited, and those fixed-size digests are sorted and folded into a final SHA-256 after the walk, which keeps the result independent of directory enumeration order while holding a flat 32 bytes per entry however long the paths are; exposes it as `POST /api/servers/:server/fingerprint` taking `{"ignore": string}` and returning `{"fingerprint", "files", "duration_ms"}` with a 60s deadline that maps to HTTP 504 with a warning log and request_id.
- Why: the panel skips automated backups whose content has not changed (spec: panel repo, docs/superpowers/specs/2026-08-22-backup-content-fingerprint-design.md); the previous disk-usage fingerprint could neither ignore log churn nor detect same-size edits.
- Files: `server/filesystem/fingerprint.go`, `server/filesystem/fingerprint_test.go`, `router/router_server_fingerprint.go`, `router/router.go`.
- Conflict risk on rebase: low; two new files plus one appended route line.
