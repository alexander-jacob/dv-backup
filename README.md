# dv-backup

## What it does

Docker Compose stacks run on VMs that are created by Terraform and deployed by GitLab CI pipelines. Compose files, `.env` secrets and images all come from the pipeline, so a VM can be tainted, re-applied and re-deployed at any time. The only state that is lost is the data in Docker volumes. `dv-backup` backs up and restores that volume data. Moving archives between hosts is out of scope: the operator copies archives with `scp`.

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

## Install

```bash
VERSION=v0.1.0
curl -fsSLO https://github.com/alexander-jacob/dv-backup/releases/download/${VERSION}/dv-backup-linux-amd64
curl -fsSLO https://github.com/alexander-jacob/dv-backup/releases/download/${VERSION}/checksums.txt
sha256sum --check --ignore-missing checksums.txt
install -m 0755 dv-backup-linux-amd64 /usr/local/bin/dv-backup
```

## Usage

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

### Restore flows

```
before deploy:  taint → apply → scp archive → dv-backup restore → run pipeline
after deploy:   taint → apply → run pipeline (or compose up --no-start) → scp archive → dv-backup restore [--force] → start stack
```

In the after-deploy flow `--force` is normally required, even with `compose up --no-start`: Docker copies image content into a new volume at container **create** time, and database images initialise their data directory on first start.

Specifically, when an image has files at a volume's mount path, Docker copies them into a new empty named volume at container **create** time, before any start (verified with `docker create` of `nginx:alpine` mounting a fresh volume at `/usr/share/nginx/html`: the volume held `index.html` and `50x.html` without the container ever starting). Consequently restore after `compose up --no-start` sees a non-empty volume for such images and requires `--force`.

## How it works

Volume data is read and written only through a short-lived helper container running GNU tar, using the Docker API directly (create, attach stdout/stdin, start, wait for exit code, remove) rather than shelling out to the `docker` CLI:

```
backup:   docker run --rm --log-driver none -v <vol>:/data:ro <image> tar --numeric-owner --xattrs --xattrs-include=* --acls --sparse -C /data -cpf - .
restore:  docker run --rm --log-driver none -i -v <vol>:/data <image> tar --numeric-owner --xattrs --xattrs-include=* --acls -C /data -xpf -
clear:    docker run --rm --log-driver none -v <vol>:/data <image> find /data -mindepth 1 -delete
empty?:   docker run --rm --log-driver none -v <vol>:/data:ro <image> find /data -mindepth 1 -maxdepth 1 -print -quit
```

`--xattrs-include=*` is set on both sides for symmetry: GNU tar already archives every xattr key (including `security.*`) with just `--xattrs`, but its default extraction filter only restores `user.*`, silently dropping keys such as `security.capability` unless `--xattrs-include=*` is also given on extract.

### Archive layout

An archive is one uncompressed outer tar file:

```
volumes/<volume>.tar.zst    one per volume: GNU tar stream, zstd (klauspost SpeedDefault, equivalent to zstd -3)
manifest.yaml               written after all volumes (needs sizes and checksums)
README.md                   human-readable rendering of the manifest
```

The manifest is the source of truth; the README is a rendering of it for humans. Example `manifest.yaml`:

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

## Safety and limitations

- Archives are not encrypted and are created with mode `0600`.
- Containers using a selected volume in state `running`, `paused` or `restarting` are stopped and restarted afterwards; a previously `paused` container comes back `running`.
- Restart does not wait for health checks: an application container started before its database is ready may exit and only recover through its own restart policy.
- A `SIGKILL` of `dv-backup` leaves containers stopped and possibly a helper container behind; clean up with `docker start` of the containers listed in the output, and `docker rm -f $(docker ps -aq --filter label=dv-backup.helper)`.
- Image pulls are anonymous; the Docker API does not read `~/.docker/config.json`, so for a private registry the operator must `docker pull` the image first.
- Bind mounts and anonymous volumes are not backed up; `stat` only reports them.
- Compose `external: true` volumes are backed up like any other named volume, but they carry no Compose labels, so `--project` never selects them: name them explicitly or use an unfiltered backup.
- Disk space: during a backup the output directory needs roughly the final archive size plus the largest compressed volume, because each volume is compressed to a temporary file before it is appended to the archive. `backup` warns when the selected volumes' disk usage exceeds the free space.
- `--no-stop` does not guarantee consistency; affected volumes are marked `consistent: false` in the manifest and flagged by `stat --archive`.
- Rootless Docker and Windows containers are not supported or tested.
- The default helper image `debian:13-slim` must be bumped when Debian 13 reaches end of life.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error (nothing or only part done — see output) |
| 2 | Invalid usage |
| 3 | Operation finished, but stopped containers could not be restarted |

## Development

```bash
go test ./...
go test -tags integration ./internal/dockerx/ ./internal/integration/
```

Integration tests create and touch only resources named `dv-backup-test-*`. **Never run an unfiltered `backup` or a forced `restore` on a workstation**: this project is developed on a machine with hundreds of unrelated volumes and containers, and both commands act on real Docker state.
