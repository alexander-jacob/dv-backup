# dv-backup — Design

- **Date:** 2026-09-17
- **Status:** Approved in brainstorming, pending spec review
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
      --verify-only             Verify manifest and checksums, change nothing

  Global:
      --image <ref>             Helper image (env DV_BACKUP_IMAGE, default debian:13-slim)
  -h, --help                    Usage, explanation, examples for both restore flows
      --version                 Print version
```

- `--help`, `-h` and running without arguments print usage. Every command has its own `--help` (cobra).
- `--project` and volume names combine as a union filter; a name that matches nothing is an error.
- Archive file name: `dv-backup-<hostname>-<UTC yyyymmddThhmmssZ>.tar`, e.g. `dv-backup-vm-app-01-20260917T120000Z.tar`.

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
backup:   docker run --rm -v <vol>:/data:ro <image> tar --numeric-owner --xattrs --acls --sparse -C /data -cpf - .
restore:  docker run --rm -i -v <vol>:/data <image> tar --numeric-owner --xattrs --acls -C /data -xpf -
clear:    docker run --rm -v <vol>:/data <image> find /data -mindepth 1 -delete
empty?:   docker run --rm -v <vol>:/data:ro <image> find /data -mindepth 1 -maxdepth 1 -print -quit
```

Implemented via the Docker API (create, attach stdout/stdin, start, wait for exit code, remove), not by shelling out to the `docker` CLI. Non-zero exit codes and stderr output of the helper are surfaced as errors.

- Default image `debian:13-slim` (GNU tar), overridable with `--image` / `DV_BACKUP_IMAGE` (e.g. for a private registry mirror). The image is pulled if missing. The resolved image digest is recorded in the manifest.

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
volumes/<volume>.tar.zst    one per volume: GNU tar stream, zstd level 3
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

- Live: table of named volumes (name, Compose project, driver, size, containers with running/stopped state). Sizes come from the Docker disk-usage API. Warnings section lists anonymous volumes and writable bind mounts of existing containers, both "not backed up".
- `--archive`: archive metadata, volume table (name, project, uncompressed size, compressed size, consistent flag), and a warning line for each inconsistent volume.

### 7.2 backup

1. **Preflight:** Docker reachable; helper image available (pull if missing); output directory writable; warn if the sum of volume sizes exceeds free space in the output directory.
2. **Select:** named volumes, filtered by `--project` / names. Nothing selected → error, no file written.
3. **Stop** (unless `--no-stop`): collect all *running* containers using any selected volume; stop each once, with its own configured stop timeout; remember the list.
4. **Archive:** write to `<name>.tar.partial`; for each volume stream helper-tar → zstd → sha256 → temp file → outer tar. After all volumes, write `manifest.yaml` and `README.md`, close, rename to `<name>.tar`.
5. **Any volume failure fails the whole backup**: remove `.partial` and temp files, exit non-zero.
6. **Always restart** exactly the containers stopped in step 3 — on success, error, `SIGINT` or `SIGTERM` — using a context that is not the cancelled one. Remove helper containers. Restart failures are reported per container and produce exit code 3.

A `SIGKILL` of `dv-backup` cannot be handled; containers may remain stopped. This is documented.

### 7.3 restore

1. **Read and verify:** read `manifest.yaml`, check `format_version`, validate names, select volumes by names/`--project` (unknown name → error), verify sha256 of every selected volume. `--verify-only` exits here.
2. **Plan** (pure function over current volume states):

   | Target state | Without `--force` | With `--force` |
   |---|---|---|
   | missing | create + unpack | create + unpack |
   | exists, empty, not used by running container | unpack | unpack |
   | exists, has data | **blocked** | stop running containers using it, clear, unpack |
   | used by a running container | **blocked** | stop running containers using it, clear, unpack |

   If any selected volume is blocked, print the full plan with reasons and exit 1 **without changing anything**.
3. **Execute** per volume in manifest order: create with driver, driver options and labels from the manifest when missing; if the volume exists and its labels differ from the manifest, warn (labels cannot be changed on existing volumes). Stopping, clearing and unpacking as planned.
4. **Failure mid-restore:** stop processing, print restored / failed / untouched volumes and the exact command to retry the failed and untouched ones with `--force`.
5. **Always restart** containers stopped in step 3, same rules as backup.

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

**Integration tests** (build tag `integration`, real Docker, resources named `dv-backup-test-<random>`, cleaned up with `t.Cleanup`):
- round trip of the spike's edge-case volume with the spike's metadata comparison (ownership, modes, setgid/sticky, symlinks, hardlinks, FIFO, unicode, mtimes, sparse allocation)
- Compose labels present on restored volumes
- running container is stopped, backed up and restarted; also when the backup fails
- `SIGINT` during backup (binary as subprocess): containers restarted, no `.tar` or `.partial` left
- corrupted archive: restore refuses before any Docker change
- blocked plan without `--force`; overwrite with `--force`
- restore after `docker compose up --no-start` (see open question 10.1)

## 9. CI and release

- **GitHub Actions** on push and pull request: `go vet`, `golangci-lint`, unit tests, integration tests on `ubuntu-latest`.
- **GoReleaser** on tags `v*`: static binaries for `linux/amd64` and `linux/arm64`, `checksums.txt`, version injected via `-ldflags` (shown by `--version` and in the manifest `tool` field).
- README documents installing a pinned release with `curl` + checksum verification.

Repository files: `README.md`, `LICENSE` (MIT), `go.mod`, `.golangci.yml`, `.goreleaser.yaml`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`.

## 10. Open questions and known limitations

1. **Image content copy-up timing:** when an image has files at a volume's mount path, Docker copies them into a new empty volume. It is not verified whether this happens at container *create* or first *start*. If at create, restore after `compose up --no-start` sees a non-empty volume and requires `--force`. To be settled by the integration test and documented in `--help`.
2. **xattrs/ACLs:** requested from GNU tar but not covered by the spike; the integration test adds a `user.*` xattr where the filesystem supports it.
3. **Bind mounts and anonymous volumes** are not backed up (reported by `stat` only).
4. **Consistency with `--no-stop`** is not guaranteed; flagged in manifest and `stat --archive`.
5. **Helper image tag** `debian:13-slim` must be bumped when Debian 13 reaches end of life.
