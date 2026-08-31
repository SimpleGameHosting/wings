# SGH Wings Patches

This fork tracks upstream `pterodactyl/wings` with a short, linear patch series.
Base: upstream v1.13.3 @ 6987d5e6f0612133e031255a99655e822d30579a.
The fork was rebased from ae6c629 onto the v1.13.3 release on 2026-08-29 after reviewing all 27 SGH commits with `git range-diff`.
The previous history is preserved at `codex/pre-audit-rebase-9022f24` until the rebased branch has been canaried.
Deployable builds are tagged `v1.13.3-sgh.<n>`.

Every SGH modification MUST be registered here before its work is considered complete.

## Rebase runbook (on upstream activity)

1. `git fetch upstream`
2. Pick the new base: the newest upstream release tag, or a pinned `develop` commit when tags lag on security updates.
3. `git rebase --onto <new-base> <old-base> sgh`
4. Resolve conflicts patch by patch; each entry below lists its touched files.
5. Run `gofmt -l . && go vet ./... && CGO_ENABLED=1 go test -race ./... && CGO_ENABLED=0 go build ./...` on Linux or in a Go container because Wings does not build natively on macOS.
   The race detector needs cgo, while the build uses `CGO_ENABLED=0` to match published binaries.
6. Update the Base line above, tag `v<upstream>-sgh.<n+1>`, push with `--force-with-lease`.
7. Canary one node, then roll the fleet via `playbooks/pterodactyl/wings_update`.

## Patches

### remote: ReportCrash panel callback

- What: adds `ReportCrash` to the remote client (interface + implementation) with `CrashReportRequest`.
- Why: the panel's crash analyzer needs authoritative crash detection (spec: panel repo, docs/superpowers/specs/2026-08-20-crash-analyzer-design.md).
- Files: `remote/http.go`, `remote/types.go`, `remote/servers.go`, `remote/servers_test.go`.
- Conflict risk on rebase: low; appends only.

### server: report crashes to the panel

- What: `OnStateChange` captures uptime before resource reset and passes that immutable value through `handleServerCrash` to the callback payload.
  `handleServerCrash` fires `ReportCrash` in a fire-and-forget goroutine with a 30-second deadline before the restart-throttle check.
- Why: the panel crash analyzer needs authoritative reports for unattended and throttled crashes.
  Passing uptime as part of the crash event prevents a later offline transition from rewriting an earlier callback while its goroutine is waiting to run.
- Files: `server/crash.go`, `server/server.go`, `server/crash_test.go`, `router/router_server_backup_test.go`.
- Conflict risk on rebase: low-medium; touches two lines inside `OnStateChange`.

### ci: sgh branch triggers, upstream watch, govulncheck

- What: `push.yaml`, `release.yaml`, and CodeQL target the `sgh` branch and SGH release tags.
  The build, Dockerfile, vulnerability scan, and release jobs all use Go 1.25.14.
  Third-party workflow actions and govulncheck v1.7.0 are pinned rather than floating by major tag or `latest`.
  CI enforces formatting, vet, normal tests, race tests, and both release architectures.
  Release builds strip the leading `v` before embedding `system.Version`, produce checksums, and only run for `v*-sgh.*` tags.
  Docker build contexts exclude Git metadata and local `dist/` artifacts.
  Builder and runtime container images are pinned to reviewed multi-architecture manifest digests.
  Dependabot targets `sgh` for Go modules, GitHub Actions, and Docker image digest updates.
  Scheduled upstream-activity issues and weekly vulnerability scans keep the fork current.
- Why: Kane's fork constraints - active upstream security tracking without waiting on a maintenance-mode upstream.
- Files: `.github/workflows/*.yaml`, `.github/dependabot.yaml`, `Dockerfile`, `.dockerignore`.
- Conflict risk on rebase: low; upstream rarely touches workflows beyond dependabot bumps.

### deps: security updates (govulncheck)

- What: toolchain go1.24.1 -> go1.25.14.
  The `go` line was raised from 1.23.0 to 1.25.0 by `go mod tidy`, which the new dependency graph requires.
  `golang.org/x/crypto` moved from v0.41.0 to v0.55.0.
  `golang.org/x/net` moved from v0.42.0 to v0.57.0.
  `golang.org/x/text` moved from v0.28.0 to v0.41.0.
  `github.com/ulikunitz/xz` moved from v0.5.14 to v0.5.15.
  `go mod tidy` also carried `golang.org/x/sync`, `golang.org/x/sys`, and `golang.org/x/term` forward transitively.
- Why: the original govulncheck run found 53 reachable vulnerabilities, including 39 in the Go 1.24 standard library.
  No Go 1.24 release fixes those standard-library findings because that release line has left its security-support window.
  Go 1.25.14 cleared every reachable standard-library finding.
  The 2026-08-30 audit also caught `GO-2026-6303` in the SSH server path and fixed it by upgrading `golang.org/x/crypto` to v0.55.0.
  The current pinned scan reports only the two documented Moby exceptions below.
- Not fixed (BLOCKED): `github.com/docker/docker` v28.3.3+incompatible still trips `GO-2026-4887` and `GO-2026-4883`.
  These are the Moby AuthZ oversized-body bypass and plugin-privilege off-by-one issues.
  Neither advisory has a fixed release under the `github.com/docker/docker` module path.
  The only published fixes use the renamed `github.com/moby/moby/v2` module at v2.0.0-beta.8 or newer.
  Migrating Wings to that beta major-version rewrite requires a separate design and compatibility task.
- Files: `go.mod`, `go.sum`, `.github/workflows/push.yaml`, `.github/workflows/release.yaml`, `.github/workflows/govulncheck.yaml`, `Dockerfile`.
- Conflict risk on rebase: low for module and workflow version changes.
  Regenerate `go.sum` with `go mod tidy` and rerun govulncheck after every rebase.

### ci: hardening

- What: `upstream-watch.yaml` uses a concurrency group so overlapping runs cannot file duplicate issues.
  Its base and latest-tag extraction fail explicitly when patch metadata is missing or malformed.
  `govulncheck.yaml` has least-privilege permissions and filters JSON findings down to reachable function traces.
  The scan subtracts only the OSV IDs committed in `.govulncheck-exceptions` and fails for every unexpected reachable finding.
  Push artifacts are gated on the actual `sgh` branch.
- Why: unattended security jobs must fail closed for malformed metadata while allowing only reviewed, currently unfixable findings.
  Artifact builds must be published from the branch this fork actually deploys.
- Files: `.github/workflows/upstream-watch.yaml`, `.github/workflows/govulncheck.yaml`, `.govulncheck-exceptions`, `.github/workflows/push.yaml`.
- Conflict risk on rebase: low; upstream rarely touches these workflow files beyond dependabot version bumps, and `.govulncheck-exceptions` is fork-only.

### vet: drop unreachable returns

- What: deletes four unreachable `return` statements that followed `panic(...)` in `NewFs()` and `router.Configure`.
  This has no behavior change.
- Why: `go vet ./...` is part of the fork's verification gate and must pass cleanly.
- Files: `server/filesystem/filesystem_test.go`, `router/router.go`.
- Conflict risk on rebase: low.
  Drop this patch if upstream removes the same lines.

### vet: stop copying mutexes (ResourceUsage, Configuration, Download)

- What: splits the lock out of the three types `go vet` flagged for copying a `sync.RWMutex` by value without changing their JSON payloads.
  `ResourceUsage` is now a plain snapshot value guarded by an unexported `resourceTracker`.
  `Configuration` embeds plain `ConfigurationData`, and `SyncWithConfiguration` replaces only that data while retaining the live lock.
  `Server.Config()` now returns a deep, isolated snapshot so direct field reads cannot race with Panel syncs and callers cannot mutate live nested maps or slices.
  Live suspension changes go through `Server.SetSuspended` under the configuration lock.
  `Download.MarshalJSON` takes a pointer receiver so an active download never copies its mutex.
  `Manager.All()` also returns an isolated collection snapshot for cron and other callers.
  Tests cover race-free configuration reads, snapshot isolation, wholesale replacement, download progress, resource snapshots, manager snapshots, and JSON compatibility.
- Why: these findings were real concurrency bugs rather than style warnings.
  `Download.MarshalJSON` could copy a locked mutex and either race with progress writes or hang a download-list response.
  Assigning an entire decoded `Configuration` could overwrite live mutex waiter state and strand readers forever.
  Returning `ResourceUsage` with an embedded mutex copied synchronization state into every API and websocket snapshot.
- Files: `server/resources.go`, `server/configuration.go`, `server/server.go`, `server/manager.go`, `server/mounts.go`, `server/backup.go`, `router/router_server.go`, `router/router_server_backup_test.go`, `router/downloader/downloader.go`, `server/resources_test.go`, `server/configuration_test.go`, `server/server_test.go`, `server/manager_test.go`, `router/downloader/downloader_test.go`.
- Conflict risk on rebase: low-medium.
  Configuration field additions belong in `ConfigurationData`, while locking remains in the `Configuration` wrapper.
  Recheck `SyncWithConfiguration`, API serialization, and `Download.MarshalJSON` on every upstream conflict.

### server: content fingerprint endpoint

- What: adds `Filesystem.Fingerprint(ctx, ignore)` using the archiver's gitignore matcher and deterministic metadata hashing.
  The walk includes empty directories and symlink targets, supports negated ignore rules, and skips Unix sockets exactly as archive generation does.
  Archive generation now writes explicit empty-directory headers, reads symlink targets relative to the validated walk descriptor, and preserves nested symlink paths.
  `POST /api/servers/:server/fingerprint` accepts `{"ignore": string}` and returns `{"fingerprint", "files", "duration_ms"}`.
  Its 60-second walk deadline derives from the HTTP request context and maps deadline expiry to HTTP 504 with a warning and request ID.
  The 60-second Wings deadline and the panel's 65-second Guzzle timeout in `DaemonServerRepository::getFingerprint` must move together.
- Why: the panel skips automated backups whose archived content has not changed.
  The previous disk-usage fingerprint could neither ignore log churn nor detect same-size edits.
  The design is in the panel repo at `docs/superpowers/specs/2026-08-22-backup-content-fingerprint-design.md`.
- Files: `server/filesystem/archive.go`, `server/filesystem/archive_test.go`, `server/filesystem/fingerprint.go`, `server/filesystem/fingerprint_test.go`, `router/router_server_fingerprint.go`, `router/router_server_fingerprint_test.go`, `router/router.go`.
- Conflict risk on rebase: low-medium.
  The fingerprint route is additive, while archive parity changes share the upstream archiver and must retain their end-to-end tar tests.

### remote/server: player events panel callback

- What: matches running-server console lines against Panel-served join and failed-join matchers, buffers them under a 20-per-minute limit, and posts batches every five seconds.
  Malformed nil matchers are ignored instead of panicking the console path.
  Player-event text is normalized and truncated at valid UTF-8 boundaries within the Panel's byte limits.
  The cron chunks requests at 20 events, deduplicates each drain, drops overflow, and uses exactly one retry for best-effort delivery.
  Each delivery has a five-second deadline so an unavailable Panel cannot occupy the shared cron indefinitely.
- Why: the panel's friction checkpoints need authoritative first-connection and failed-join detection.
  The design is in the panel repo at `docs/superpowers/specs/2026-08-22-friction-checkpoints-design.md`.
- Files: `remote/types.go`, `remote/http.go`, `remote/servers.go`, `remote/types_test.go`, `remote/http_test.go`, `remote/servers_test.go`, `remote/player_events_fixture_test.go`, `remote/testdata/player_events_minecraft_java.json`, `router/router_server_backup_test.go`, `server/player_events.go`, `server/player_events_test.go`, `server/server.go`, `server/listeners.go`, `internal/cron/player_events_cron.go`, `internal/cron/cron.go`, `internal/cron/player_events_cron_test.go`.
- Conflict risk on rebase: low-medium.
  `ProcessConfiguration` and `onConsoleOutput` are upstream-owned surfaces with additive SGH changes.

### router: 404 for missing files

- What: `asFilesystemError()` also matches plain `os.ErrNotExist` (via `errors.Is`) so missing-file errors that bypass the filesystem error wrapper return 404 instead of 500.
- Why: file endpoints returned internal-server-error for simple missing paths, confusing the panel file manager and API consumers.
- Files: `router/middleware/request_error.go`, `router/middleware/request_error_test.go`.
- Conflict risk on rebase: low; one condition plus a test.

### config: default backup compression best_compression

- What: `SystemConfiguration.Backups.CompressionLevel` default changes `best_speed` -> `best_compression`.
- Why: replaces the retired `backup_compression.yml` ansible enforcement - SGH wants maximum backup compression fleet-wide; a binary default cannot drift. Note: an explicit `compression_level` in a node's config.yml still overrides this.
- Files: `config/config.go`.
- Conflict risk on rebase: low; one struct-tag value.

### server: restore archived directories correctly

- What: backup restore recreates directory entries from the archive instead of mis-handling them, so restored servers keep their directory structure.
- Why: critical fix - restores could produce incorrect/missing directories from archived backups.
- Files: `server/backup.go`, `server/backup_restore_test.go`.
- Conflict risk on rebase: low; localized to the restore path plus a test.

### router: decompress archives in the background

- What: `postServerDecompressFiles` validates the archive synchronously with the new `Filesystem.CanDecompressFile` (an open plus a header sniff), then returns HTTP 202 and runs the disk-space walk and the extraction in a background goroutine tied to the server context, reporting start, failure, and completion through daemon console lines.
  Unknown-format archives now return their 400 deterministically; previously servers without a disk limit skipped the synchronous format check entirely and failed through the generic error path.
- Why: multi-gigabyte modpack archives outlived proxy timeouts, so users saw 504s while the extraction continued invisibly and retried into double extractions (upstream issue pterodactyl/panel#2878, open since 2020).
- Files: `router/router_server_files.go`, `router/router_server_files_decompress_test.go`, `server/filesystem/compress.go`.
- Conflict risk on rebase: low-medium; the handler body is upstream-owned but self-contained, and `CanDecompressFile` is additive.

### router/environment: honest power actions during boot

- What: `postServerPower` returns HTTP 409 when a no-wait start, stop, or restart arrives while another power action holds the lock, instead of accepting the request and silently dropping it in the background goroutine; kill stays exempt and `wait_seconds` callers keep their queueing behavior.
  `SignalContainer` no longer walks a booting server to offline: a created-but-unstarted container is force removed so the boot's own failure handling settles the state, and a kill before the container exists returns an error instead of faking success.
  `Terminate` leaves a still-starting environment's state untouched for the same reason, and a kill that lands while the boot is still recreating its container can be outrun by the boot - the state stays truthful and a retried kill wins.
  The docker `Environment` client field becomes the `client.APIClient` interface, with the performant-inspect fast path guarded to the concrete client, so the termination path is testable with a fake client.
- Why: power buttons lied during boot - stops vanished behind a 202 while kills marked a booting server offline and could trip crash detection, desyncing the panel and feeding false crashes to the SGH crash analyzer (upstream issue pterodactyl/panel#5712).
- Files: `router/router_server.go`, `router/router_server_power_test.go`, `environment/docker/power.go`, `environment/docker/power_test.go`, `environment/docker/environment.go`, `environment/docker/api.go`, `environment/docker/cgroup_burst.go`.
- Conflict risk on rebase: medium; `SignalContainer`, `Terminate`, and `postServerPower` are upstream-owned surfaces, and the client field type change surfaces in any upstream change to `environment/docker`.

### files: preserve symlinks during extraction and restore

- What: `extractStream` recreates symlink entries through the new `Filesystem.OverwriteSymlink` instead of writing them out as files, `RestoreCallback` now carries `archives.FileInfo` so `restoreBackupEntry` sees link targets and restores links, and `Filesystem.Symlink` creates missing parent directories and lchowns the link itself.
  `OverwriteSymlink` replaces an existing file, link, or empty directory at the link path so repeat extraction and restore-in-place stay idempotent like regular file entries, while a non-empty directory remains an error and SFTP keeps strict fail-if-exists `Symlink` semantics.
  SFTP symlink creation shares the hardened `Symlink` helper, so links created over SFTP gain server ownership and implicit parent directories.
- Why: archive creation has preserved symlinks since the fingerprint patch, but every transfer extraction, file-manager decompression, and backup restore materialized them as empty regular files, silently breaking servers that boot through links such as Forge's `unix_args.txt` (upstream issue pterodactyl/panel#5429, upstream PR pterodactyl/wings#286).
- Files: `server/filesystem/compress.go`, `server/filesystem/compress_test.go`, `server/filesystem/filesystem.go`, `server/backup.go`, `server/backup/backup.go`, `server/backup/backup_local.go`, `server/backup/backup_s3.go`, `server/backup_restore_test.go`.
- Conflict risk on rebase: medium; the extract callback and the `RestoreCallback` signature are upstream-owned surfaces, and upstream PR #286 touches the same lines with a compatible shape.

### router: always clean up failed incoming transfers

- What: the deferred transfer completion in `postTransfers` moves into `finalizeIncomingTransfer`, which deletes a failed transfer's extracted files unconditionally, notifies the panel afterwards, and always clears the transferring flag before publishing the real status event.
  Upstream only deleted the files when the panel status call also failed, and its early return on that error left the server marked as transferring forever.
- Why: every ordinarily failed or interrupted transfer orphaned the server's full data directory on the destination node with nothing left to clean it up, leaking disk fleet-wide (upstream issue pterodactyl/panel#5555, upstream PR pterodactyl/wings#298).
- Files: `router/router_transfer.go`, `router/router_transfer_test.go`.
- Conflict risk on rebase: low-medium; the deferred body is upstream-owned, so re-apply the extraction if upstream restructures `postTransfers`.

### router: report failed transfer creation with the token UUID

- What: when `installer.New` fails for a brand-new incoming transfer, `postTransfers` now reports the failed status to the panel using the UUID parsed from the transfer token instead of dereferencing `trnsfr.Server`, which is only assigned after the installer succeeds.
- Why: any installer failure for a fresh transfer (unreachable panel, bad server configuration) dereferenced the nil server and panicked, so the source node got a blank 500 and the panel was never told the transfer failed, leaving it stuck in a transferring state (pre-existing upstream bug, still present in v1.13.3).
- Files: `router/router_transfer.go`, `router/router_transfer_test.go`.
- Conflict risk on rebase: low; one line in the error branch plus an additive end-to-end test.
