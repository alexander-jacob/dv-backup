# dv-backup — Design

- **Date:** 2026-09-17
- **Status:** Approved (2026-09-17), revised after review the same day (see 12.1). Implementation plan not yet written.
- **Repository:** https://github.com/alexander-jacob/dv-backup (public, MIT)
- **Module path:** `github.com/alexander-jacob/dv-backup`

## 1. Context

Docker Compose stacks run on VMs that are created by Terraform and deployed by GitLab CI pipelines. Compose files, `.env` secrets and images all come from the pipeline, so a VM can be tainted, re-applied and re-deployed at any time. The only state that is lost is the **data in Docker volumes**.

`dv-backup` backs up and restores that volume data. Moving archives between hosts is out of scope: the operator copies archives with `scp`.

Recovery workflows it must support:

```
before deploy:  taint → apply → scp archive → dv-backup restore → run pipeline
after deploy:   taint → apply → run pipeline (or compose up --no-start) → scp archive → dv-backup restore [--force] → start stack
```

In the after-deploy flow `--force` is normally required, even with `compose up --no-start`: Docker copies image content into a new volume at container **create** time (verified, see 10.1), and database images initialise their data directory on first start. The help text must say so.

## 2. Goals and non-goals

**Goals**
- Back up named Docker volumes into one self-describing archive file.
- Restore volumes onto a fresh or an already deployed host, byte- and metadata-faithful (ownership, modes, links, sparse files).
- Show what exists on a host and what an archive contains, without unpacking it.
- Be safe by default: consistent backups, no silent overwrites, containers always restarted.
- Ship as a single static binary; the host needs only Docker.

**Non-goals**
- Bind mounts (only reported by `stat`).
- Anonymous volumes (names are random and cannot be matched on restore; only reported by `stat`).
- Containers, images, networks, Compose files, secrets — owned by the GitLab pipeline.
- Transport, scheduling, retention, encryption — owned by the operator (`scp`, cron/CI).
- Application-aware dumps (`pg_dump` etc.).

## 3. CLI

```
dv-backup <command> [options]

  stat                          Named volumes on this host: size, driver, Compose project,
                                containers using each (running/stopped). Warns about
                                anonymous volumes and writable bind mounts (not backed up).
  stat --archive <file>         Archive manifest: host, date, volumes, sizes, sha256,
                                consistency flag. Reads only the manifest.

  backup [volume...]            Back up all named volumes, or only the listed ones.
      -o, --output <dir>        Output directory (default: current directory)
      -p, --project <name>      Only volumes of this Compose project (repeatable)
      --no-stop                 Do not stop containers; affected volumes marked inconsistent

  restore <file> [volume...]    Restore all volumes from the archive, or only the listed ones.
      -p, --project <name>      Only volumes of this Compose project (repeatable)
      --force                   Overwrite volumes that contain data or are in use;
                                stops and restarts the containers using them
      --verify-only             Verify manifest and checksums, change nothing (works without Docker)

  Global:
      --image <ref>             Helper image (env DV_BACKUP_IMAGE, default debian:13-slim)
  -h, --help                    Usage, explanation, examples for both restore flows
      --version                 Print version
```

- `--help`, `-h` and running without arguments print usage. Every command has its own `--help` (cobra).
- `--project` and volume names combine as a union filter; a name that matches nothing is an error.
- Archive file name: `dv-backup-<hostname>-<UTC yyyymmddThhmmssZ>.tar`, e.g. `dv-backup-vm-app-01-20260917T120000Z.tar`. `backup` refuses to run if that `.tar` or its `.partial` already exists (create with `O_EXCL`); it never overwrites.

### Output

- Human-readable text; tables for `stat`, one line per step otherwise. Errors and warnings go to stderr, everything else to stdout. No `--json` or `--quiet` in v1.
- `backup` and `restore` print one line when a volume starts and one when it finishes (uncompressed and compressed size, elapsed time), plus a line per container stopped and restarted. Volumes can be many GB, so the start line must appear before the data is streamed. No byte-level progress bar in v1.
- `restore` prints the plan (volume, current state, action, reason) before executing it, and again with the outcome per volume (restored / failed / untouched) when it stops early.

## 4. Architecture

```
cmd/dv-backup/main.go   CLI wiring: commands, flags, help, exit codes (cobra)
internal/dockerx/       Interface + implementation over github.com/moby/moby/client:
                        list volumes (with sizes), containers using a volume,
                        stop/start, create volume, run helper container with streamed stdio
internal/manifest/      Manifest types, YAML read/write, validation, README.md rendering
internal/archive/       Outer tar writer/reader, per-volume zstd + sha256 streaming
internal/backup/        Select → stop → archive each volume → always restart
internal/restore/       Verify → plan (pure function) → execute
internal/stat/          Live host stat and archive stat rendering
```

Dependencies: `github.com/moby/moby/client`, `github.com/spf13/cobra`, `github.com/klauspost/compress/zstd`, `go.yaml.in/yaml/v3`. Everything else from the standard library. Built with `CGO_ENABLED=0`.

`backup`, `restore` and `stat` depend on the `dockerx` interface, not on the Docker client directly, so their logic is unit-testable with a fake.

## 5. Data access mechanism

Volume data is read and written **only** through a short-lived helper container running GNU tar:

```
backup:   docker run --rm -v <vol>:/data:ro <image> tar --numeric-owner --xattrs --xattrs-include=* --acls --sparse -C /data -cpf - .
restore:  docker run --rm -i -v <vol>:/data <image> tar --numeric-owner --xattrs --xattrs-include=* --acls -C /data -xpf -
clear:    docker run --rm -v <vol>:/data <image> find /data -mindepth 1 -delete
empty?:   docker run --rm -v <vol>:/data:ro <image> find /data -mindepth 1 -maxdepth 1 -print -quit
```

Implemented via the Docker API (create, attach stdout/stdin, start, wait for exit code, remove), not by shelling out to the `docker` CLI. Non-zero exit codes and stderr output of the helper are surfaced as errors.

- Default image `debian:13-slim` (verified 2026-09-17: GNU tar 1.35 with `--acls`, `--xattrs`, `--selinux`, `--sparse`; findutils 4.10), overridable with `--image` / `DV_BACKUP_IMAGE` (e.g. for a private registry mirror). The image is pulled **anonymously** if missing; the Docker API does not read `~/.docker/config.json`, so for a registry that needs credentials the operator must `docker pull` the image first. The README and the pull error message must say so. The resolved image digest is recorded in the manifest.

### Why not Docker's copy API (`CopyFromContainer`/`CopyToContainer`)

Spike on 2026-09-17 (Docker 29.8.1) with a test volume containing a `999:999 0700` root, `0600` files, setgid and sticky directories, relative and absolute symlinks, hardlinks, a FIFO, a unicode filename, fixed mtimes, a 100 MB sparse file and 200 MB random data. Compared owner, mode, type, link count, mtime, symlink target and MD5 of every entry:

| Variant | Result |
|---|---|
| Copy API, `SourcePath=/data` → `DestinationPath=/` | Identical except sparse file restored fully allocated (0 KB → 102,400 KB) |
| Copy API, `SourcePath=/data/.` → `DestinationPath=/data` | Volume root ownership lost (`999:999 0700` → `0:0 0755`), plus sparse expansion |
| GNU tar helper container | **Identical in every respect** |

GNU tar is the only fully faithful variant; the extra runtime (seconds per volume) is acceptable for backups.

## 6. Archive format

Outer file: uncompressed tar.

```
volumes/<volume>.tar.zst    one per volume: GNU tar stream, zstd (klauspost `SpeedDefault`, equivalent to `zstd -3`)
manifest.yaml               written after all volumes (needs sizes and checksums)
README.md                   human-readable rendering of the manifest
```

- Each `volumes/<volume>.tar.zst` is streamed to a temporary file in the output directory (sha256 computed while writing), then appended to the outer tar, because tar headers require the entry size up front. Disk needed during backup ≈ final archive size + largest compressed volume.
- `stat --archive` reads the outer tar with `archive/tar` over an `*os.File`; entry contents are skipped via `Seek`, so only tar headers and `manifest.yaml` are read.
- Volume names (Docker allows `[a-zA-Z0-9][a-zA-Z0-9_.-]+`) are used as file names. On read, names are validated against that pattern; archive entries outside `manifest.yaml`, `README.md` and `volumes/<valid-name>.tar.zst` are rejected.

### Manifest (`manifest.yaml`)

```yaml
format_version: 1
tool: dv-backup v0.1.0
created_at: 2026-09-17T12:00:00Z
host: vm-app-01
docker_version: 29.8.1
helper_image: debian:13-slim@sha256:…
volumes:
  - name: n8n_n8n_data
    driver: local
    driver_opts: {}
    labels:
      com.docker.compose.project: n8n
      com.docker.compose.volume: n8n_data
    size_bytes: 52428800          # uncompressed tar stream
    archive:
      path: volumes/n8n_n8n_data.tar.zst
      size_bytes: 9834211
      sha256: 3f1c…
    consistent: true              # false if --no-stop and a using container was running
    containers:
      - name: n8n-n8n-1
        image: docker.n8n.io/n8nio/n8n
        compose_service: n8n
        was_running: true
```

- `format_version` is checked on read; unknown versions are refused with a clear message.
- `README.md` is generated from the same in-memory manifest; tools must only ever read `manifest.yaml`.

## 7. Behavior

Principle: check everything first, then change things; never leave a state that looks complete but is not.

### 7.1 stat

- Live: table of named volumes (name, Compose project, driver, size, containers with their state). Sizes come from the Docker disk-usage API (measured 2026-09-17: 7 s for 645 volumes, acceptable). Helper containers of dv-backup (label `dv-backup.helper`) are never listed as users of a volume. Warnings section lists anonymous volumes, writable bind mounts of existing containers (both "not backed up") and stray dv-backup helper containers with the command to remove them.
- `--archive`: archive metadata, volume table (name, project, uncompressed size, compressed size, consistent flag), and a warning line for each inconsistent volume.

### 7.2 backup

1. **Preflight:** Docker reachable; helper image available (pull if missing); output directory writable; warn if the sum of volume sizes exceeds free space in the output directory.
2. **Select:** named volumes, filtered by `--project` / names. Nothing selected → error, no file written.
3. **Stop** (unless `--no-stop`): collect all *in-use* containers using any selected volume; stop each once, with its own configured stop timeout; remember the list. **In use** means `State` is `running`, `paused` or `restarting`: a restarting (crash-looping) container writes between restarts, a paused one holds files open and resumes writing after unpause. `docker stop` handles all three (verified on 29.8.1: a paused container stops cleanly and ends up `exited`). Stopped containers come back `running`; a previously paused container is not re-paused. This is documented.
4. **Archive:** write to `<name>.tar.partial`; for each volume stream helper-tar → zstd → sha256 → temp file → outer tar. After all volumes, write `manifest.yaml` and `README.md`, close, rename to `<name>.tar`.
5. **Any volume failure fails the whole backup**: remove `.partial` and temp files, exit non-zero.
6. **Always restart** exactly the containers stopped in step 3 — on success, error, `SIGINT` or `SIGTERM` — using a context that is not the cancelled one, in ascending order of their original `State.StartedAt` (so dependencies started earlier come back first). Remove helper containers. Restart failures are reported per container and produce exit code 3.

Restart does not wait for health checks. An application container that is started before its database is ready may exit and only recovers through its own restart policy. This is documented as a limitation.

A `SIGKILL` of `dv-backup` cannot be handled; containers may remain stopped and a helper container may remain. This is documented, together with the cleanup commands (`docker start` of the containers listed in the output, `docker rm -f $(docker ps -aq --filter label=dv-backup.helper)`).

### 7.3 restore

1. **Read and verify:** read `manifest.yaml`, check `format_version`, validate names, select volumes by names/`--project` (unknown name → error), verify sha256 of every selected volume. `--verify-only` exits here.
2. **Plan** (pure function over current volume states):

   | Target state | Without `--force` | With `--force` |
   |---|---|---|
   | missing | create + unpack | create + unpack |
   | exists, empty, not in use | unpack | unpack |
   | exists, has data | **blocked** | stop in-use containers using it, clear, unpack |
   | in use (container `running`, `paused` or `restarting`) | **blocked** | stop in-use containers using it, clear, unpack |

   "In use" has the same meaning as in backup step 3. If any selected volume is blocked, print the full plan with reasons and exit 1 **without changing anything**. If a volume exists and its driver, driver options or labels differ from the manifest, the plan shows a warning (these cannot be changed on an existing volume); this does not block.
3. **Stop** all containers the plan marks for stopping, before the first volume is changed (same rules as backup step 3); remember the list.
4. **Execute** per volume in manifest order: create with driver, driver options and labels from the manifest when missing; clear and unpack as planned.
5. **Failure mid-restore:** stop processing, print restored / failed / untouched volumes and the exact command to retry the failed and untouched ones with `--force`.
6. **Always restart** containers stopped in step 3, same rules as backup step 6.

### 7.4 Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error (nothing or only part done — see output) |
| 2 | Invalid usage |
| 3 | Operation finished, but stopped containers could not be restarted |

## 8. Testing

**Unit tests** (fake `dockerx`):
- manifest YAML round trip, `format_version` refusal, name validation (`../`, empty, invalid characters)
- restore planning: table-driven over all states × `--force`
- backup stop/restart bookkeeping on success, error and context cancellation
- outer archive round trip, sha256 mismatch detection, `.partial` cleanup
- selection filters (`--project`, names, unknown names)
- restart ordering by `StartedAt`; "in use" classification for `running`, `paused`, `restarting`, `exited`, `created`
- `--no-stop` with helper exit code 1 → warning and `consistent: false`; exit code 2 → failure
- archive created with mode `0600`; refusal when the target file exists

**Integration tests** (build tag `integration`, real Docker, resources named `dv-backup-test-<random>`, cleaned up with `t.Cleanup`):
- round trip of the spike's edge-case volume with the spike's metadata comparison (ownership, modes, setgid/sticky, symlinks, hardlinks, FIFO, unicode, mtimes, sparse allocation)
- Compose labels present on restored volumes
- running container is stopped, backed up and restarted; also when the backup fails
- paused container is stopped, backed up and comes back running
- `SIGINT` during backup (binary as subprocess): containers restarted, no `.tar` or `.partial` left
- corrupted archive: restore refuses before any Docker change
- blocked plan without `--force`; overwrite with `--force`
- restore after `docker compose up --no-start` into a volume populated by image copy-up: blocked without `--force`, succeeds with it (see 10.1)
- xattrs: a `user.*` key and, where the kernel allows it in the test environment, `security.capability` on a file, both present after restore (see 10.2)

## 9. CI and release

- **GitHub Actions** on push and pull request: `go vet`, `golangci-lint`, unit tests, integration tests on `ubuntu-latest`.
- **GoReleaser** on tags `v*`: static binaries for `linux/amd64` and `linux/arm64`, `checksums.txt`, version injected via `-ldflags` (shown by `--version` and in the manifest `tool` field).
- README documents installing a pinned release with `curl` + checksum verification.

Repository files: `README.md`, `LICENSE` (MIT), `go.mod`, `.golangci.yml`, `.goreleaser.yaml`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`.

## 10. Open questions and known limitations

1. **Image content copy-up timing — settled (2026-09-17, Docker 29.8.1):** when an image has files at a volume's mount path, Docker copies them into a new empty named volume at container **create** time, before any start (verified with `docker create` of `nginx:alpine` mounting a fresh volume at `/usr/share/nginx/html`: the volume held `index.html` and `50x.html` without the container ever starting). Consequently restore after `compose up --no-start` sees a non-empty volume for such images and requires `--force`. Documented in `--help` and README; the integration test covers it.
2. **xattrs/ACLs — settled (2026-09-17, integration test `TestXattrsRoundTrip`):** GNU tar 1.35 already archives every xattr key, including `security.*`, on creation with only `--xattrs`. The loss was on **extraction**: GNU tar's default xattr filter on `-x` only restores `user.*`, so `security.capability` was silently dropped when a `.tar` was unpacked back into a volume. Adding `--xattrs-include='*'` to both the backup and restore helper's `tar` invocation (`BackupHelper`, `RestoreHelper` in `internal/dockerx/dockerx.go`) fixes it — the flag is set on both sides for symmetry, though it is only load-bearing on restore. Verified: a `user.*` xattr and `security.capability` (VFS_CAP_REVISION_2) both round-trip correctly. `--acls` was already present and is unaffected. Not fixed by this flag: `trusted.*` (and possibly `security.selinux`) records present in an archive may still fail to apply on restore, because the helper container runs without `CAP_SYS_ADMIN`; GNU tar only warns (`Cannot set 'trusted.*' extended attribute for file ...: Operation not permitted`) and continues rather than failing the run.
3. **Bind mounts and anonymous volumes** are not backed up (reported by `stat` only).
4. **Consistency with `--no-stop`** is not guaranteed; flagged in manifest and `stat --archive`.
5. **Helper image tag** `debian:13-slim` must be bumped when Debian 13 reaches end of life.
6. **Compose `external: true` volumes** carry no Compose labels, so `--project` never selects them; they must be named explicitly or included via an unfiltered backup. Documented.
7. **No dependency wait on restart** (see 7.2 step 6).
8. **Linux daemons only.** Windows containers are not supported.
9. **Docker volume name pattern:** Docker's pattern lives in daemon code (`daemon/names`), not in the api or client modules, and could not be checked. The spec's pattern requires at least two characters; the implementer must confirm on a scratch daemon whether one-character names are accepted (`docker volume create a` on the workstation is **not** allowed: a volume `a` may already exist) and adjust the read-side validation so that no valid Docker name is rejected.

## 11. Implementation notes (verified facts for a fresh implementer)

Everything in this section was checked on 2026-09-17 with `go doc` against the listed module versions or observed in the spike. Re-verify with `go doc` if you upgrade modules.

### 11.1 Docker Go client

- Use **`github.com/moby/moby/client`** (verified `v0.6.0`) with types from **`github.com/moby/moby/api`** (verified `v1.56.0`). Do **not** use the older `github.com/docker/docker/client`; examples found online mostly use that one and its method signatures differ.
- This client uses an *options struct in, result struct out* style for almost every call. Verified signatures:

  ```go
  client.New(client.FromEnv)                                                          // honours DOCKER_HOST etc.; negotiates API version by default
  (*Client).Close() error
  (*Client).Info(ctx, client.InfoOptions{}) (client.SystemInfoResult, error)          // .Info.Name = daemon hostname
  (*Client).ServerVersion(ctx, client.ServerVersionOptions{}) (client.ServerVersionResult, error) // .Version
  (*Client).VolumeList(ctx, client.VolumeListOptions{Filters: f}) (client.VolumeListResult, error) // .Items []volume.Volume
  (*Client).VolumeInspect(ctx, name, client.VolumeInspectOptions{}) (client.VolumeInspectResult, error) // .Volume
  (*Client).VolumeCreate(ctx, client.VolumeCreateOptions{Name, Driver, DriverOpts, Labels}) (client.VolumeCreateResult, error)
  (*Client).DiskUsage(ctx, client.DiskUsageOptions{Volumes: true}) (client.DiskUsageResult, error) // .Volumes.Items[].UsageData
  (*Client).ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f}) (client.ContainerListResult, error) // .Items []container.Summary
  (*Client).ContainerInspect(ctx, id, client.ContainerInspectOptions{}) (client.ContainerInspectResult, error) // .Container.State.StartedAt
  (*Client).ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: nil})        // nil = container's own StopTimeout
  (*Client).ContainerStart(ctx, id, client.ContainerStartOptions{})
  (*Client).ContainerCreate(ctx, client.ContainerCreateOptions{Config, HostConfig, Name}) (client.ContainerCreateResult, error) // .ID
  (*Client).ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream, Stdin, Stdout, Stderr}) (client.ContainerAttachResult, error)
  (*Client).ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit}) client.ContainerWaitResult // .Result <-chan container.WaitResponse, .Error <-chan error
  (*Client).ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
  (*Client).ImageInspect(ctx, ref, ...client.ImageInspectOption) (client.ImageInspectResult, error) // embeds image.InspectResponse (.RepoDigests)
  (*Client).ImagePull(ctx, ref, client.ImagePullOptions{}) (client.ImagePullResponse, error) // call .Wait(ctx); RegistryAuth left empty (anonymous pull, see §5)
  ```

- `client.Filters` is `map[string]map[string]bool`; build with `make(client.Filters).Add("volume", name)`.
- `container.Summary.Mounts []container.MountPoint` has `Type`, `Name` (volume name), `Source`, `Destination`, `RW` — enough to find volume users and writable bind mounts without inspecting each container.
- `ContainerList` filter `volume=<name>` returns containers mounting that volume. Use `All: true`, then classify by `State`: `running`, `paused`, `restarting` are "in use" (see 7.2 step 3); everything else is not. Exclude containers with label `dv-backup.helper`.
- Demultiplex attached output with `stdcopy.StdCopy(stdout, stderr, reader)` from **`github.com/moby/moby/api/pkg/stdcopy`** (not `client/pkg/stdcopy`, which does not exist).

### 11.2 Helper container recipe

The helper is the only code that touches volume data. Get every detail right:

1. **Create** with:
   - `Config.Image` = helper image, `Config.Entrypoint` = `[]string{"tar"}` (or `find`), `Config.Cmd` = the arguments. Always set `Entrypoint` explicitly so an overridden `--image` with its own entrypoint cannot change the behaviour.
   - `Config.User = "0:0"`: tar must run as root to restore ownership.
   - `Config.Tty = false`: stdout and stderr are multiplexed, and the stream is binary-safe.
   - For restore: `Config.OpenStdin = true`, `Config.StdinOnce = true`, `Config.AttachStdin = true`.
   - `Config.Labels = {"dv-backup.helper": "true"}` and `Name = "dv-backup-helper-<random hex>"`, so stray helpers can be identified.
   - `HostConfig.NetworkMode = "none"`.
   - `HostConfig.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: "/data", ReadOnly: <true for backup/empty-check>, VolumeOptions: &mount.VolumeOptions{NoCopy: true}}}`. **`NoCopy: true` is mandatory:** without it, Docker copies image content at `/data` into an empty volume, which would corrupt a restore or make an empty volume look non-empty.
   - **Do not set `AutoRemove`.** The container could disappear before the exit code is read. Remove it explicitly in a `defer` with `Force: true`, using a non-cancelled context.
2. **Attach** (`Stream: true`, `Stdout: true`, `Stderr: true`, plus `Stdin: true` for restore) **before** starting, so no output is lost.
3. **Wait** with `WaitConditionNextExit` **before** starting (the documented way to avoid missing a fast exit).
4. **Start.**
5. **Stream:**
   - Backup: `stdcopy.StdCopy(pipelineWriter, stderrBuf, attach.Reader)`.
   - Restore: copy the decompressed volume tar into `attach.Conn`, then call `attach.CloseWrite()` so tar sees EOF. Drain output with `StdCopy` concurrently.
   - `stderrBuf` keeps only the last 64 KiB, for error messages.
6. **Read the exit code** from `wait.Result` (or `wait.Error`), then close the attach response.

GNU tar exit codes: `0` success; `1` means files changed while being read (with `--create`), so the archive is not an exact copy; `2` fatal error. Rules:
- containers stopped (normal backup): any non-zero exit → the volume fails → the backup fails
- `--no-stop`: exit `1` → warning, volume marked `consistent: false`; exit `2` → the backup fails
- restore, clear, empty check: any non-zero exit is an error

### 11.3 Volume classification

- **Anonymous volume:** has the label `com.docker.volume.anonymous` (verified on Docker 29.8.1: present on exactly the 77 hex-named volumes of the workstation), **or** its name matches `^[0-9a-f]{64}$` (older daemons). Anonymous volumes are excluded from backup and listed as warnings by `stat`.
- **Compose project of a volume:** label `com.docker.compose.project`. Compose also sets `com.docker.compose.volume` and `com.docker.compose.version`. Restore copies **all** labels verbatim. Compose recognises an existing volume through these labels; without them it warns that the volume "already exists but was not created by Docker Compose".
- **Writable bind mounts** (for the `stat` warning): `MountPoint.Type == "bind" && RW`. Skip `Source` values under `/var/run`, `/run`, `/dev`, `/proc` and `/sys`.

### 11.4 Other decisions fixed for implementation

- **Host name** in manifest and file name: the Docker daemon's `Info.Name` (verified: returns the daemon's hostname), not `os.Hostname()`. It must describe the Docker host even with a remote `DOCKER_HOST`.
- **Archive file permissions:** `0600`. Volume data often contains secrets (database contents, credentials). The README must state that archives are **not encrypted**.
- **Temporary files:** `<output-dir>/.dv-backup-<volume>-<random>.tmp`, removed on success and failure.
- **Filename sanitising:** Docker host names are safe for file names; still, replace any character outside `[A-Za-z0-9._-]` with `-`.
- **Helper image digest in manifest:** first `RepoDigests` entry from `ImageInspect`; if empty (locally built image), record the image ID.
- **Volume size in `stat`:** `DiskUsage` with `Volumes: true`; `Volume.UsageData` is a pointer that may be `nil`, and `UsageData.Size` is `-1` when unknown; show `?` in both cases.
- **Rootless Docker:** not supported or tested. `--numeric-owner` inside a user namespace maps IDs differently. Document as a limitation.

### 11.5 Safety when developing on a workstation

The author's workstation has ~645 volumes and ~187 containers from unrelated projects, including running databases.
- Integration tests must create and select **only** resources prefixed `dv-backup-test-` and must always pass explicit volume names to `backup`/`restore`. **Never** run an unfiltered `backup` or `restore --force` from a test.
- Tests must never call `docker volume prune`, `docker system prune`, or remove anything they did not create.

## 12. Decision log

These were debated and settled with the maintainer. Do not reopen them without new evidence.

| Decision | Rejected alternatives | Reason |
|---|---|---|
| Volumes only | containers (`export`/`commit`), images, bind mounts | Pipeline recreates everything but volume data; `export`/`commit` exclude volume contents and lose config; bind mount host paths change on new VMs (runner checkout paths). |
| Go | Bash script | Stop/restart and restore state logic must be reliable (`defer`, real errors, unit tests); single binary with no host dependencies besides Docker. |
| GNU tar in helper container | Docker copy API; reading `/var/lib/docker/volumes` directly | Spike: only GNU tar was fully faithful; direct access needs root, only works with the `local` driver and depends on Docker internals. |
| Stop containers by default, `--no-stop` opt-out | always live; opt-in stop | File-level copies of running databases can be unusable (PostgreSQL docs: file system level backup requires the server to be shut down). |
| One failing volume fails the whole backup | skip and mark | An incomplete archive is only discovered at restore time. |
| All named volumes by default | only in-use volumes; explicit list | VMs only contain pipeline-created volumes; explicit lists go stale. |
| Restore supports before- and after-deploy flows | one flow only | Maintainer requirement. |
| Per-volume `.tar.zst` inside an uncompressed outer tar | one big compressed tar; directory of files | One file to `scp`; the manifest is readable without decompressing; per-volume checksums and selective restore. |
| YAML manifest + generated README.md | JSON | Human readability was the original requirement; YAML was only rejected earlier for Bash. |
| Local file in/out, transport by operator | Azure Blob, Azure Files, GitLab | Maintainer copies archives with `scp`. |
| Public on GitHub, MIT | internal GitLab; Apache-2.0; GPL-3.0 | Open source; MIT is shortest for a small CLI. |
| Name `dv-backup` ("docker volume backup") | `dc-backup` | Renamed by maintainer. |
| Anonymous image pull only | read `~/.docker/config.json` credential store | The Docker API does not apply CLI credentials; reading the credential store (and credential helpers) adds a dependency for a rare case. Operator pre-pulls instead. |
| `paused` and `restarting` count as in use | only `running` | Both can write to the volume; `docker stop` handles them (verified). |

### 12.1 Review revisions (2026-09-17)

An independent review after approval led to these changes: copy-up timing settled (10.1); `paused`/`restarting` treated as in use (7.2, 7.3, 11.1); restore stops all containers before changing volumes (7.3); anonymous image pull and pre-pull requirement (5); output and progress specified (3); refusal to overwrite an existing archive (3); `--verify-only` works without Docker (3); driver/driver-option mismatch warning (7.3); no dependency wait on restart, helper cleanup and Linux-only limitations (7.2, 10); external Compose volumes (10); zstd level naming (6); additional tests (8); unverified items listed under 10.2 and 10.9.

## Appendix A: Spike fixture scripts

Reuse these in the integration tests (port to Go or run through the helper image). **Populate** runs as root with the volume mounted at `/data`:

```bash
set -e
cd /data
mkdir -p pgdata empty setgid sticky
echo 17 > pgdata/PG_VERSION
chmod 0600 pgdata/PG_VERSION
chown -R 999:999 pgdata && chmod 0700 pgdata
chown 1000:1000 empty && chmod 0750 empty
ln -s pgdata/PG_VERSION rel-link && chown -h 999:999 rel-link
ln -s /etc/passwd abs-link
echo hard > hard1 && ln hard1 hard2
chmod 2775 setgid && chown 0:999 setgid
chmod 1777 sticky
printf '#!/bin/sh\necho hi\n' > exec.sh && chmod 0755 exec.sh
echo x > "file with spaces & ümlaut"
mkfifo fifo
truncate -s 100M sparse.img
head -c 200M /dev/urandom > big.bin
touch -h -d '2020-01-02 03:04:05' pgdata/PG_VERSION exec.sh hard1 rel-link big.bin
chown 999:999 /data && chmod 0700 /data
```

**Inspect** produces a comparable listing. Two volumes are identical when their outputs are identical:

```bash
cd /data && find . -printf '%U:%G %#m %y %n %Ts %l %p\n' | sort -k7
echo "--- sums"; find . -type f -exec md5sum {} + | sort -k2
echo "--- disk usage (sparse check)"; du -sk sparse.img big.bin 2>/dev/null
```

For tests, use a smaller `big.bin` (e.g. 5 MB) so they stay fast. The sparse file can stay at 100 MB, since it takes no disk space.

## Appendix B: Development environment (as of 2026-09-17)

- Go `1.27.1` installed at `~/sdk/go1.27.1` (on `PATH`); Docker Engine `29.8.1`; IDE GoLand.
- Repository `github.com/alexander-jacob/dv-backup` exists and is empty; the local repo has the remote `origin` but **has not been pushed**. Existing commits use the author's work email. Ask the maintainer before the first push whether to rewrite them to a GitHub `noreply` address.
- `.gitignore` currently covers archives (`*.tar`, `*.tar.zst`), `.env*`, `.idea/`, `.vscode/`, OS files and logs. Add Go entries (`/dv-backup` binary, `/dist/`, `coverage.out`, `*.test`) during scaffolding.
- Spike code lived in a session scratchpad and is gone; Appendix A preserves what matters.
