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
