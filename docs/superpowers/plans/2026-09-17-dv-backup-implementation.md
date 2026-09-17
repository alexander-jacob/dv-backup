# dv-backup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `dv-backup`, a single static Go binary that backs up named Docker volumes into one self-describing tar archive and restores them faithfully, stopping and restarting the containers that use them.

**Architecture:** All volume data flows through a short-lived helper container running GNU tar, driven over the Docker API. A `dockerx.Docker` interface isolates every Docker call so `backup`, `restore` and `stat` are unit-tested against an in-memory fake; the real client and helper runner are covered by integration tests against a live daemon. The archive is an uncompressed outer tar holding one zstd-compressed tar stream per volume plus a YAML manifest.

**Tech Stack:** Go 1.27, `github.com/moby/moby/client` v0.6.0 + `github.com/moby/moby/api` v1.56.0, `github.com/containerd/errdefs` v1.0.0 (error classification, already a transitive dependency of the client), `github.com/spf13/cobra` v1.10.2, `github.com/klauspost/compress` v1.20.0 (zstd), `go.yaml.in/yaml/v3` v3.0.5. Standard library for everything else. Tests use only the `testing` package.

**Spec:** `docs/superpowers/specs/2026-09-17-dv-backup-design.md`. Read it fully before starting. Section numbers below (`§5`, `§11.2`) refer to it.

## Global Constraints

- Module path `github.com/alexander-jacob/dv-backup`; binary `dv-backup`; `go 1.27` in `go.mod`.
- Build with `CGO_ENABLED=0`. Linux daemons only. Rootless Docker unsupported.
- Only the dependencies listed above. No testify, no other helpers.
- Volume data is touched **only** through helper containers (§5). Never read `/var/lib/docker`.
- Helper containers: `Entrypoint` set explicitly, `User "0:0"`, `Tty false`, `NetworkMode "none"`, mount at `/data` with `NoCopy: true`, label `dv-backup.helper=true`, name `dv-backup-helper-<hex>`, **no** `AutoRemove` (§11.2).
- "In use" container = state `running`, `paused` or `restarting` (§7.2 step 3).
- Archive: outer uncompressed tar, `volumes/<name>.tar.zst` entries first, then `manifest.yaml`, then `README.md`; file mode `0600`; never overwrite an existing `.tar` or `.partial` (§3, §6).
- Archive file name `dv-backup-<host>-<yyyymmddThhmmssZ>.tar`, host from the daemon's `Info.Name`, characters outside `[A-Za-z0-9._-]` replaced by `-` (§11.4).
- Image pulls are anonymous; the error message tells the operator to `docker pull` first (§5).
- Exit codes: 0 success, 1 error, 2 usage, 3 finished but containers could not be restarted (§7.4).
- Errors and warnings to stderr, everything else to stdout (§3 Output).
- **Workstation safety (§11.5):** the development machine has ~645 volumes and ~187 containers from other projects. Integration tests create and touch only resources named `dv-backup-test-*`, always pass explicit volume names, never call any prune, never remove what they did not create. Never run an unfiltered `backup` or `restore --force` manually on this machine either.
- Do not push to GitHub. Appendix B of the spec asks the maintainer to settle the commit email before the first push.

## Before you start (fresh-context checklist)

1. `go version` must print `go1.27.x`. `docker info --format '{{.ServerVersion}}'` must print `29.x`. Both are installed on the workstation; the Docker daemon is local.
2. The Go modules are already in the module cache; `go doc github.com/moby/moby/client Client.ContainerAttach` works offline. Use `go doc` whenever a signature in this plan looks off. Do **not** consult examples for `github.com/docker/docker/client`; that is a different, older API.
3. `golangci-lint` and `goreleaser` are **not** installed locally. Install the linter with `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` in Task 17. GoReleaser runs only in CI.
4. Unit tests: `go test ./...`. Integration tests: `go test -tags integration ./internal/integration/ -v` (Task 16 onward). Run `go vet ./...` before each commit.
5. Commit after every task with the message given in the task. Do not push.
6. Every package gets a `doc.go` or a package comment on its main file; keep exported identifiers documented (the linter checks this).

## File structure

```
go.mod, go.sum
cmd/dv-backup/main.go                 cobra commands, exit codes, help text, signal context
cmd/dv-backup/main_test.go            exit-code and usage tests
internal/version/version.go           Version variable set by -ldflags
internal/manifest/manifest.go         Manifest types, Parse, Marshal, Validate, ValidName, VolumePath
internal/manifest/manifest_test.go
internal/manifest/readme.go           RenderREADME
internal/manifest/readme_test.go
internal/archive/writer.go            Writer: .partial file, per-volume temp+zstd+sha256, Finish, Abort
internal/archive/reader.go            Reader: header scan, Manifest, Verify, OpenVolume
internal/archive/archive_test.go
internal/dockerx/dockerx.go           Docker interface, Volume/Container/Mount/Helper types, helper specs
internal/dockerx/fake.go              Fake implementation for unit tests
internal/dockerx/fake_test.go
internal/dockerx/client.go            real implementation (moby client)
internal/dockerx/helper.go            RunHelper: create/attach/wait/start/stream/exit code
internal/selection/selection.go       generic name/project filter, anonymous detection
internal/selection/selection_test.go
internal/stopper/stopper.go           stop bookkeeping and ordered restart
internal/stopper/stopper_test.go
internal/backup/backup.go             backup orchestration
internal/backup/backup_test.go
internal/restore/plan.go              pure planner
internal/restore/plan_test.go
internal/restore/restore.go           restore execution
internal/restore/restore_test.go
internal/stat/stat.go                 host and archive stat rendering
internal/stat/stat_test.go
internal/integration/*_test.go        build tag `integration`, real Docker
.github/workflows/ci.yml, .github/workflows/release.yml
.golangci.yml, .goreleaser.yaml, README.md, LICENSE
```

---

### Task 1: Module scaffold, version package, root command

**Files:**
- Create: `go.mod`, `internal/version/version.go`, `cmd/dv-backup/main.go`, `cmd/dv-backup/main_test.go`, `LICENSE`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `version.Version string` (default `"dev"`), `version.Tool() string` returning `"dv-backup " + Version`; `run(args []string, stdout, stderr io.Writer) int` in package main, used by all later CLI tests.

- [ ] **Step 1: Create the module and fetch dependencies**

```bash
cd /home/alex/projects/dje/infrastructure/dv-backup
go mod init github.com/alexander-jacob/dv-backup
go get github.com/moby/moby/client@v0.6.0 github.com/moby/moby/api@v1.56.0 \
       github.com/containerd/errdefs@v1.0.0 github.com/spf13/cobra@v1.10.2 \
       github.com/klauspost/compress@v1.20.0 go.yaml.in/yaml/v3@v3.0.5
```

Check that `go.mod` starts with `go 1.27` (edit it if `go mod init` wrote a patch version, e.g. `go 1.27.1`; either is fine).

- [ ] **Step 2: Append Go entries to `.gitignore`**

```
# Go
/dv-backup
/dist/
coverage.out
*.test
```

- [ ] **Step 3: Write the MIT license**

`LICENSE`: the standard MIT text with `Copyright (c) 2026 Alexander Jacob`.

- [ ] **Step 4: Write the version package**

```go
// Package version holds the build version injected at release time.
package version

// Version is set with -ldflags "-X github.com/alexander-jacob/dv-backup/internal/version.Version=v1.2.3".
var Version = "dev"

// Tool returns the tool identifier recorded in manifests, e.g. "dv-backup v0.1.0".
func Tool() string { return "dv-backup " + Version }
```

- [ ] **Step 5: Write the failing CLI test**

`cmd/dv-backup/main_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--version"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "dv-backup ") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestNoArgsPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage, got %q", out.String())
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `go test ./cmd/dv-backup/`
Expected: FAIL, `undefined: run`.

- [ ] **Step 7: Write the minimal main**

`cmd/dv-backup/main.go`:

```go
// Command dv-backup backs up and restores named Docker volumes.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alexander-jacob/dv-backup/internal/version"
)

// exitError carries an exit code from a command's RunE to run().
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func fail(code int, err error) error { return &exitError{code: code, err: err} }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRoot(ctx, stdout, stderr)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(stderr, "error:", ee.err)
		return ee.code
	}
	// Anything else came from cobra itself: unknown command, bad flag, wrong arg count.
	fmt.Fprintln(stderr, "error:", err)
	return 2
}

func newRoot(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "dv-backup",
		Short:         "Back up and restore named Docker volumes",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetContext(ctx)
	root.SetVersionTemplate("dv-backup {{.Version}}\n")
	return root
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS (three tests).

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum .gitignore LICENSE internal/version cmd/dv-backup
git commit -m "Scaffold module, version package and root command"
```

---

### Task 2: Manifest types, parsing and validation

**Files:**
- Create: `internal/manifest/manifest.go`, `internal/manifest/manifest_test.go`

**Interfaces:**
- Produces:
  - `const FormatVersion = 1`
  - `type Manifest struct { FormatVersion int; Tool string; CreatedAt time.Time; Host, DockerVersion, HelperImage string; Volumes []Volume }`
  - `type Volume struct { Name, Driver string; DriverOpts, Labels map[string]string; SizeBytes int64; Archive Entry; Consistent bool; Containers []Container }`
  - `type Entry struct { Path string; SizeBytes int64; SHA256 string }`
  - `type Container struct { Name, Image, ComposeService string; WasRunning bool }`
  - `func Parse(data []byte) (*Manifest, error)`; `func (m *Manifest) Marshal() ([]byte, error)`; `func (m *Manifest) Validate() error`; `func (m *Manifest) Find(name string) (*Volume, bool)`
  - `func ValidName(name string) bool`; `func VolumePath(name string) string` (returns `volumes/<name>.tar.zst`); `func (v Volume) Project() string`

- [ ] **Step 1: Write the failing tests**

```go
package manifest

import (
	"strings"
	"testing"
	"time"
)

const sample = `format_version: 1
tool: dv-backup v0.1.0
created_at: 2026-09-17T12:00:00Z
host: vm-app-01
docker_version: 29.8.1
helper_image: debian:13-slim@sha256:abc
volumes:
  - name: n8n_n8n_data
    driver: local
    driver_opts: {}
    labels:
      com.docker.compose.project: n8n
      com.docker.compose.volume: n8n_data
    size_bytes: 52428800
    archive:
      path: volumes/n8n_n8n_data.tar.zst
      size_bytes: 9834211
      sha256: 3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c
    consistent: true
    containers:
      - name: n8n-n8n-1
        image: docker.n8n.io/n8nio/n8n
        compose_service: n8n
        was_running: true
`

func TestParseSample(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.Host != "vm-app-01" || m.DockerVersion != "29.8.1" {
		t.Fatalf("header fields wrong: %+v", m)
	}
	if !m.CreatedAt.Equal(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %v", m.CreatedAt)
	}
	v, ok := m.Find("n8n_n8n_data")
	if !ok {
		t.Fatal("volume not found")
	}
	if v.Project() != "n8n" || v.Archive.SizeBytes != 9834211 || !v.Consistent {
		t.Fatalf("volume fields wrong: %+v", v)
	}
	if len(v.Containers) != 1 || v.Containers[0].ComposeService != "n8n" || !v.Containers[0].WasRunning {
		t.Fatalf("containers wrong: %+v", v.Containers)
	}
}

func TestRoundTrip(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if again.Volumes[0].Archive.SHA256 != m.Volumes[0].Archive.SHA256 || again.Volumes[0].Labels["com.docker.compose.project"] != "n8n" {
		t.Fatalf("round trip lost data: %+v", again)
	}
}

func TestRefusesUnknownFormatVersion(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(sample, "format_version: 1", "format_version: 2", 1)))
	if err == nil || !strings.Contains(err.Error(), "format_version 2") {
		t.Fatalf("expected format_version error, got %v", err)
	}
	_, err = Parse([]byte(strings.Replace(sample, "format_version: 1\n", "", 1)))
	if err == nil {
		t.Fatal("expected error for missing format_version")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "n8n_n8n_data", "my-vol.1", "0abc"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "-x", "_x", "a b", "a/b", "a\x00b", "über"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"bad name":       strings.Replace(sample, "name: n8n_n8n_data", "name: ../etc", 1),
		"path mismatch":  strings.Replace(sample, "path: volumes/n8n_n8n_data.tar.zst", "path: volumes/other.tar.zst", 1),
		"short checksum": strings.Replace(sample, "sha256: 3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c", "sha256: abc", 1),
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	dup := strings.Replace(sample, "volumes:\n", "volumes:\n  - name: n8n_n8n_data\n    archive:\n      path: volumes/n8n_n8n_data.tar.zst\n      sha256: 3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c\n", 1)
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/manifest/`
Expected: FAIL, undefined `Parse`, `ValidName`.

- [ ] **Step 3: Write the implementation**

`internal/manifest/manifest.go`:

```go
// Package manifest defines the archive manifest (manifest.yaml) and its rendering.
package manifest

import (
	"fmt"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"
)

// FormatVersion is the manifest format this build reads and writes.
const FormatVersion = 1

// LabelComposeProject is the Compose label that names a volume's project.
const LabelComposeProject = "com.docker.compose.project"

// nameRE mirrors Docker's restricted name rule. It deliberately allows
// one-character names (see spec §10.9) so that no valid Docker name is rejected.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidName reports whether name is a valid Docker volume name and therefore
// safe to use as a path component inside the archive.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// VolumePath returns the archive entry path for a volume.
func VolumePath(name string) string { return "volumes/" + name + ".tar.zst" }

// Manifest describes one archive.
type Manifest struct {
	FormatVersion int       `yaml:"format_version"`
	Tool          string    `yaml:"tool"`
	CreatedAt     time.Time `yaml:"created_at"`
	Host          string    `yaml:"host"`
	DockerVersion string    `yaml:"docker_version"`
	HelperImage   string    `yaml:"helper_image"`
	Volumes       []Volume  `yaml:"volumes"`
}

// Volume describes one backed-up volume.
type Volume struct {
	Name       string            `yaml:"name"`
	Driver     string            `yaml:"driver"`
	DriverOpts map[string]string `yaml:"driver_opts"`
	Labels     map[string]string `yaml:"labels"`
	SizeBytes  int64             `yaml:"size_bytes"`
	Archive    Entry             `yaml:"archive"`
	Consistent bool              `yaml:"consistent"`
	Containers []Container       `yaml:"containers"`
}

// Entry locates and checksums a volume's compressed stream inside the archive.
type Entry struct {
	Path      string `yaml:"path"`
	SizeBytes int64  `yaml:"size_bytes"`
	SHA256    string `yaml:"sha256"`
}

// Container records a container that used the volume at backup time.
type Container struct {
	Name           string `yaml:"name"`
	Image          string `yaml:"image"`
	ComposeService string `yaml:"compose_service,omitempty"`
	WasRunning     bool   `yaml:"was_running"`
}

// Project returns the Compose project of the volume, or "".
func (v Volume) Project() string { return v.Labels[LabelComposeProject] }

// Marshal renders the manifest as YAML.
func (m *Manifest) Marshal() ([]byte, error) { return yaml.Marshal(m) }

// Parse decodes and validates a manifest. It refuses unknown format versions.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest.yaml: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("manifest format_version %d is not supported by this dv-backup (supports %d); use a matching dv-backup release",
			m.FormatVersion, FormatVersion)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks names, paths and checksums so that archive entries can be trusted as file names.
func (m *Manifest) Validate() error {
	seen := make(map[string]bool, len(m.Volumes))
	for _, v := range m.Volumes {
		if !ValidName(v.Name) {
			return fmt.Errorf("manifest: invalid volume name %q", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("manifest: duplicate volume %q", v.Name)
		}
		seen[v.Name] = true
		if v.Archive.Path != VolumePath(v.Name) {
			return fmt.Errorf("manifest: volume %q: unexpected archive path %q", v.Name, v.Archive.Path)
		}
		if len(v.Archive.SHA256) != 64 {
			return fmt.Errorf("manifest: volume %q: invalid sha256 %q", v.Name, v.Archive.SHA256)
		}
	}
	return nil
}

// Find returns the volume with the given name.
func (m *Manifest) Find(name string) (*Volume, bool) {
	for i := range m.Volumes {
		if m.Volumes[i].Name == name {
			return &m.Volumes[i], true
		}
	}
	return nil, false
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/manifest/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manifest
git commit -m "Add manifest types, parsing and validation"
```

---

### Task 3: README rendering for the archive

**Files:**
- Create: `internal/manifest/readme.go`, `internal/manifest/readme_test.go`

**Interfaces:**
- Produces: `func RenderREADME(m *Manifest) string`; `func FormatBytes(n int64) string` (e.g. `9.4 MiB`, `?` for negative).

- [ ] **Step 1: Write the failing test**

```go
package manifest

import (
	"strings"
	"testing"
)

func TestRenderREADME(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	m.Volumes[0].Consistent = false
	out := RenderREADME(m)
	for _, want := range []string{"# dv-backup archive", "vm-app-01", "2026-09-17T12:00:00Z", "n8n_n8n_data", "n8n", "50.0 MiB", "9.4 MiB", "**not consistent**", "n8n-n8n-1", "not encrypted"} {
		if !strings.Contains(out, want) {
			t.Errorf("README lacks %q:\n%s", want, out)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{-1: "?", 0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 52428800: "50.0 MiB", 3 << 30: "3.0 GiB"}
	for n, want := range cases {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/manifest/ -run 'README|FormatBytes'`
Expected: FAIL, undefined `RenderREADME`.

- [ ] **Step 3: Write the implementation**

`internal/manifest/readme.go`:

```go
package manifest

import (
	"fmt"
	"strings"
	"time"
)

// FormatBytes renders a byte count for humans. Negative means unknown.
func FormatBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// RenderREADME produces the human-readable README.md stored next to manifest.yaml.
// It is derived from the manifest and must never be parsed by tools.
func RenderREADME(m *Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dv-backup archive\n\n")
	fmt.Fprintf(&b, "- Host: %s\n- Created: %s\n- Docker: %s\n- Tool: %s\n- Helper image: %s\n\n",
		m.Host, m.CreatedAt.UTC().Format(time.RFC3339), m.DockerVersion, m.Tool, m.HelperImage)
	fmt.Fprintf(&b, "This archive is **not encrypted**. Volume data often contains secrets; store and copy it accordingly.\n\n")
	fmt.Fprintf(&b, "## Volumes\n\n| Volume | Project | Driver | Data | Compressed | Consistent | Containers |\n|---|---|---|---|---|---|---|\n")
	for _, v := range m.Volumes {
		cons := "yes"
		if !v.Consistent {
			cons = "**not consistent**"
		}
		var cs []string
		for _, c := range v.Containers {
			state := "stopped"
			if c.WasRunning {
				state = "running"
			}
			cs = append(cs, fmt.Sprintf("%s (%s)", c.Name, state))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			v.Name, v.Project(), v.Driver, FormatBytes(v.SizeBytes), FormatBytes(v.Archive.SizeBytes), cons, strings.Join(cs, ", "))
	}
	fmt.Fprintf(&b, "\n## Restore\n\n```\ndv-backup restore <this file>            # fresh host\ndv-backup restore --force <this file>    # host with existing data\ndv-backup stat --archive <this file>     # inspect\n```\n")
	return b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/manifest/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manifest
git commit -m "Render README.md from the manifest"
```

---
### Task 4: Archive writer

**Files:**
- Create: `internal/archive/writer.go`, `internal/archive/archive_test.go`

**Interfaces:**
- Produces:
  - `const ManifestPath = "manifest.yaml"`, `const ReadmePath = "README.md"`
  - `func FileName(host string, t time.Time) string`
  - `type Writer`; `func NewWriter(dir, host string, now time.Time) (*Writer, error)`; `(*Writer).Path() string`; `(*Writer).AddVolume(name string, src io.Reader) (manifest.Entry, int64, error)` returning the entry and the uncompressed byte count; `(*Writer).Finish(m *manifest.Manifest) error`; `(*Writer).Abort() error`

- [ ] **Step 1: Write the failing tests**

`internal/archive/archive_test.go` (the reader tests in Task 5 are added to this file later):

```go
package archive

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

var testTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// fakeTarStream returns bytes that stand in for a helper's tar output; the
// writer treats the stream as opaque.
func fakeTarStream(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%7)
	}
	return b
}

func newManifest(entries ...manifest.Volume) *manifest.Manifest {
	return &manifest.Manifest{FormatVersion: manifest.FormatVersion, Tool: "dv-backup test", CreatedAt: testTime,
		Host: "vm-app-01", DockerVersion: "29.8.1", HelperImage: "debian:13-slim@sha256:x", Volumes: entries}
}

func TestFileName(t *testing.T) {
	got := FileName("vm app/01", testTime)
	if got != "dv-backup-vm-app-01-20260917T120000Z.tar" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterProducesReadableTar(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "vm-app-01", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dv-backup-vm-app-01-20260917T120000Z.tar.partial")); err != nil {
		t.Fatalf("partial file missing: %v", err)
	}
	data := fakeTarStream(1, 100_000)
	entry, n, err := w.AddVolume("vol_a", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) || entry.Path != "volumes/vol_a.tar.zst" || entry.SizeBytes <= 0 {
		t.Fatalf("entry = %+v, n = %d", entry, n)
	}
	m := newManifest(manifest.Volume{Name: "vol_a", Driver: "local", SizeBytes: n, Archive: entry, Consistent: true})
	if err := w.Finish(m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path()); err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	info, _ := os.Stat(w.Path())
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".dv-backup-*"))
	partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	if len(leftovers) != 0 || len(partials) != 0 {
		t.Fatalf("temp files left: %v %v", leftovers, partials)
	}

	// Read back with archive/tar and check order, checksum and content.
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		body, _ := io.ReadAll(tr)
		switch hdr.Name {
		case "volumes/vol_a.tar.zst":
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != entry.SHA256 || int64(len(body)) != entry.SizeBytes {
				t.Fatal("checksum or size mismatch")
			}
			dec, _ := zstd.NewReader(bytes.NewReader(body))
			plain, _ := io.ReadAll(dec)
			dec.Close()
			if !bytes.Equal(plain, data) {
				t.Fatal("decompressed data differs")
			}
		case ManifestPath:
			if _, err := manifest.Parse(body); err != nil {
				t.Fatalf("manifest in archive invalid: %v", err)
			}
		case ReadmePath:
			if !strings.Contains(string(body), "vol_a") {
				t.Fatal("README does not mention volume")
			}
		}
	}
	if strings.Join(names, ",") != "volumes/vol_a.tar.zst,manifest.yaml,README.md" {
		t.Fatalf("entry order %v", names)
	}
}

func TestWriterRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, FileName("h", testTime))
	if err := os.WriteFile(final, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, "h", testTime); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("expected refusal, got %v", err)
	}
	os.Remove(final)
	if err := os.WriteFile(final+".partial", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, "h", testTime); err == nil {
		t.Fatal("expected refusal for existing .partial")
	}
}

func TestWriterAbortRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "h", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AddVolume("v", bytes.NewReader(fakeTarStream(2, 10))); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("directory not clean: %v", files)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestWriterAddVolumeErrorCleansTemp(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "h", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AddVolume("v", failingReader{}); err == nil {
		t.Fatal("expected error")
	}
	_ = w.Abort()
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("directory not clean: %v", files)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/archive/`
Expected: FAIL, undefined `NewWriter`.

- [ ] **Step 3: Write the implementation**

`internal/archive/writer.go`:

```go
// Package archive reads and writes the outer dv-backup tar file.
package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

// Entry names inside the outer tar.
const (
	ManifestPath = "manifest.yaml"
	ReadmePath   = "README.md"
)

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// FileName returns the archive file name for a host and time (UTC).
func FileName(host string, t time.Time) string {
	return "dv-backup-" + unsafeChars.ReplaceAllString(host, "-") + "-" + t.UTC().Format("20060102T150405Z") + ".tar"
}

// Writer builds an archive in <dir>/<name>.tar.partial and renames it on Finish.
type Writer struct {
	dir     string
	final   string
	partial string
	f       *os.File
	tw      *tar.Writer
	done    bool
}

// NewWriter creates the .partial file. It refuses to touch an existing .tar or .partial.
func NewWriter(dir, host string, now time.Time) (*Writer, error) {
	final := filepath.Join(dir, FileName(host, now))
	partial := final + ".partial"
	if _, err := os.Stat(final); err == nil {
		return nil, fmt.Errorf("%s already exists; refusing to overwrite", final)
	}
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s already exists (unfinished backup?); remove it first", partial)
		}
		return nil, fmt.Errorf("create %s: %w", partial, err)
	}
	return &Writer{dir: dir, final: final, partial: partial, f: f, tw: tar.NewWriter(f)}, nil
}

// Path returns the final archive path.
func (w *Writer) Path() string { return w.final }

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// AddVolume compresses src with zstd into a temp file (hashing as it goes) and
// appends it to the outer tar. It returns the manifest entry and the number of
// uncompressed bytes read from src.
func (w *Writer) AddVolume(name string, src io.Reader) (manifest.Entry, int64, error) {
	tmp, err := os.CreateTemp(w.dir, ".dv-backup-"+name+"-*.tmp")
	if err != nil {
		return manifest.Entry{}, 0, fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return manifest.Entry{}, 0, err
	}
	hash := sha256.New()
	counter := &countingWriter{w: io.MultiWriter(tmp, hash)}
	enc, err := zstd.NewWriter(counter, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return manifest.Entry{}, 0, err
	}
	uncompressed, err := io.Copy(enc, src)
	if err != nil {
		enc.Close()
		return manifest.Entry{}, 0, fmt.Errorf("volume %s: %w", name, err)
	}
	if err := enc.Close(); err != nil {
		return manifest.Entry{}, 0, fmt.Errorf("volume %s: compress: %w", name, err)
	}
	entry := manifest.Entry{Path: manifest.VolumePath(name), SizeBytes: counter.n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return manifest.Entry{}, 0, err
	}
	if err := w.addEntry(entry.Path, entry.SizeBytes, tmp); err != nil {
		return manifest.Entry{}, 0, fmt.Errorf("volume %s: write archive: %w", name, err)
	}
	return entry, uncompressed, nil
}

func (w *Writer) addEntry(name string, size int64, r io.Reader) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now().UTC(), Typeflag: tar.TypeReg}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := io.Copy(w.tw, r)
	return err
}

// Finish writes manifest.yaml and README.md, closes the file and renames it to its final name.
func (w *Writer) Finish(m *manifest.Manifest) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	if err := w.addEntry(ManifestPath, int64(len(data)), bytesReader(data)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	readme := []byte(manifest.RenderREADME(m))
	if err := w.addEntry(ReadmePath, int64(len(readme)), bytesReader(readme)); err != nil {
		return fmt.Errorf("write README: %w", err)
	}
	if err := w.tw.Close(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.partial, w.final); err != nil {
		return err
	}
	w.done = true
	return nil
}

// Abort removes the .partial file. Safe to call after Finish (no-op).
func (w *Writer) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	_ = w.f.Close()
	if err := os.Remove(w.partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func bytesReader(b []byte) io.Reader { return &byteSlice{b: b} }

type byteSlice struct{ b []byte }

func (s *byteSlice) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}
```

(`bytesReader` exists only to avoid importing `bytes` for one call; replacing it with `bytes.NewReader` is fine.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/archive/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/archive
git commit -m "Add archive writer with per-volume zstd and sha256"
```

---

### Task 5: Archive reader

**Files:**
- Create: `internal/archive/reader.go`
- Modify: `internal/archive/archive_test.go`

**Interfaces:**
- Produces: `type Reader`; `func Open(path string) (*Reader, error)`; `(*Reader).Manifest() *manifest.Manifest`; `(*Reader).Verify(name string) error`; `(*Reader).OpenVolume(name string) (io.ReadCloser, error)` (decompressed tar stream); `(*Reader).Close() error`.
- `Open` reads only tar headers and `manifest.yaml`; entry data is skipped via `Seek`.

- [ ] **Step 1: Add the failing tests**

Append to `internal/archive/archive_test.go`:

```go
func writeTestArchive(t *testing.T, dir string, volumes map[string][]byte) (string, *manifest.Manifest) {
	t.Helper()
	w, err := NewWriter(dir, "h", testTime)
	if err != nil {
		t.Fatal(err)
	}
	m := newManifest()
	for name, data := range volumes {
		entry, n, err := w.AddVolume(name, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		m.Volumes = append(m.Volumes, manifest.Volume{Name: name, Driver: "local", SizeBytes: n, Archive: entry, Consistent: true})
	}
	if err := w.Finish(m); err != nil {
		t.Fatal(err)
	}
	return w.Path(), m
}

func TestReaderRoundTrip(t *testing.T) {
	a, b := fakeTarStream(1, 50_000), fakeTarStream(9, 10)
	path, _ := writeTestArchive(t, t.TempDir(), map[string][]byte{"vol_a": a, "vol_b": b})
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Manifest().Volumes) != 2 {
		t.Fatalf("manifest volumes = %d", len(r.Manifest().Volumes))
	}
	for name, want := range map[string][]byte{"vol_a": a, "vol_b": b} {
		if err := r.Verify(name); err != nil {
			t.Fatalf("verify %s: %v", name, err)
		}
		rc, err := r.OpenVolume(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: data differs (err %v)", name, err)
		}
	}
	if _, err := r.OpenVolume("nope"); err == nil {
		t.Fatal("expected error for unknown volume")
	}
}

func TestReaderDetectsCorruption(t *testing.T) {
	path, _ := writeTestArchive(t, t.TempDir(), map[string][]byte{"vol_a": fakeTarStream(1, 50_000)})
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The first entry's data starts right after its 512-byte header.
	if _, err := f.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, 512+1000); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Verify("vol_a"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestReaderRejectsForeignEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evil.tar")
	f, _ := os.Create(path)
	tw := tar.NewWriter(f)
	body := []byte("x")
	_ = tw.WriteHeader(&tar.Header{Name: "volumes/../../etc/passwd.tar.zst", Size: 1, Mode: 0o600})
	_, _ = tw.Write(body)
	tw.Close()
	f.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unexpected archive entry") {
		t.Fatalf("expected rejection, got %v", err)
	}
	if _, err := Open(filepath.Join(dir, "missing.tar")); err == nil {
		t.Fatal("expected error for missing file")
	}
	if err := os.WriteFile(filepath.Join(dir, "garbage.tar"), []byte("not a tar at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "garbage.tar")); err == nil {
		t.Fatal("expected error for garbage")
	}
}

func TestReaderRequiresManifestAndEntries(t *testing.T) {
	dir := t.TempDir()
	// Manifest that names a volume whose entry is absent.
	path := filepath.Join(dir, "x.tar")
	f, _ := os.Create(path)
	tw := tar.NewWriter(f)
	m := newManifest(manifest.Volume{Name: "ghost", Archive: manifest.Entry{Path: "volumes/ghost.tar.zst", SHA256: strings.Repeat("0", 64)}})
	data, _ := m.Marshal()
	_ = tw.WriteHeader(&tar.Header{Name: ManifestPath, Size: int64(len(data)), Mode: 0o600})
	_, _ = tw.Write(data)
	tw.Close()
	f.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected missing entry error, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/archive/`
Expected: FAIL, undefined `Open`.

- [ ] **Step 3: Write the implementation**

`internal/archive/reader.go`:

```go
package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

type section struct{ off, size int64 }

// Reader gives random access to the entries of an archive.
type Reader struct {
	f       *os.File
	m       *manifest.Manifest
	entries map[string]section
}

// Open scans the tar headers, reads manifest.yaml and validates that every
// manifest volume has an entry of the recorded size. Volume data is not read.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{f: f, entries: map[string]section{}}
	var manifestData []byte
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: not a dv-backup archive: %w", path, err)
		}
		// archive/tar reads headers straight from f, so the file offset now
		// points at the entry's first data byte. Next() skips data via Seek.
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			f.Close()
			return nil, err
		}
		switch {
		case hdr.Name == ManifestPath:
			if manifestData, err = io.ReadAll(tr); err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: read manifest: %w", path, err)
			}
		case hdr.Name == ReadmePath, isVolumeEntry(hdr.Name):
		default:
			f.Close()
			return nil, fmt.Errorf("%s: unexpected archive entry %q", path, hdr.Name)
		}
		r.entries[hdr.Name] = section{off: off, size: hdr.Size}
	}
	if manifestData == nil {
		f.Close()
		return nil, fmt.Errorf("%s: no manifest.yaml (incomplete backup?)", path)
	}
	m, err := manifest.Parse(manifestData)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, v := range m.Volumes {
		s, ok := r.entries[v.Archive.Path]
		if !ok {
			f.Close()
			return nil, fmt.Errorf("%s: manifest lists volume %s but entry %s is missing", path, v.Name, v.Archive.Path)
		}
		if s.size != v.Archive.SizeBytes {
			f.Close()
			return nil, fmt.Errorf("%s: entry %s has %d bytes, manifest says %d", path, v.Archive.Path, s.size, v.Archive.SizeBytes)
		}
	}
	r.m = m
	return r, nil
}

func isVolumeEntry(name string) bool {
	rest, ok := strings.CutPrefix(name, "volumes/")
	if !ok {
		return false
	}
	base, ok := strings.CutSuffix(rest, ".tar.zst")
	return ok && manifest.ValidName(base)
}

// Manifest returns the parsed manifest.
func (r *Reader) Manifest() *manifest.Manifest { return r.m }

func (r *Reader) compressed(name string) (*manifest.Volume, *io.SectionReader, error) {
	v, ok := r.m.Find(name)
	if !ok {
		return nil, nil, fmt.Errorf("volume %s is not in the archive", name)
	}
	s := r.entries[v.Archive.Path]
	return v, io.NewSectionReader(r.f, s.off, s.size), nil
}

// Verify recomputes the sha256 of the volume's compressed entry.
func (r *Reader) Verify(name string) error {
	v, sr, err := r.compressed(name)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, sr); err != nil {
		return fmt.Errorf("volume %s: read: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.Archive.SHA256 {
		return fmt.Errorf("volume %s: checksum mismatch (manifest %s, file %s)", name, v.Archive.SHA256, got)
	}
	return nil
}

// OpenVolume returns the decompressed tar stream of a volume.
func (r *Reader) OpenVolume(name string) (io.ReadCloser, error) {
	_, sr, err := r.compressed(name)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(sr)
	if err != nil {
		return nil, fmt.Errorf("volume %s: zstd: %w", name, err)
	}
	return dec.IOReadCloser(), nil
}

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/archive/`
Expected: PASS. If `TestReaderRoundTrip` fails on the second volume with garbage data, the offset assumption is wrong for this Go version: replace the `f.Seek(0, io.SeekCurrent)` approach by wrapping `f` in a small `offsetReader` that counts bytes read and implements `Seek`, and take `off` from that counter. Verify with `go doc archive/tar Reader.Next` that skipping uses `io.Seeker`.

- [ ] **Step 5: Commit**

```bash
git add internal/archive
git commit -m "Add archive reader with checksum verification"
```

---
### Task 6: dockerx interface, types and in-memory fake

**Files:**
- Create: `internal/dockerx/dockerx.go`, `internal/dockerx/fake.go`, `internal/dockerx/fake_test.go`

**Interfaces:**
- Produces (all in package `dockerx`):

```go
const (
	StateRunning = "running"; StatePaused = "paused"; StateRestarting = "restarting"; StateExited = "exited"; StateCreated = "created"
	LabelHelper = "dv-backup.helper"; LabelAnonymous = "com.docker.volume.anonymous"
	LabelComposeProject = "com.docker.compose.project"; LabelComposeService = "com.docker.compose.service"
	DefaultImage = "debian:13-slim"; DataDir = "/data"
)
type HostInfo struct{ Name, ServerVersion string }
type Volume struct{ Name, Driver string; Options, Labels map[string]string }
type Mount struct{ Type, Name, Source, Destination string; RW bool }
type Container struct{ ID, Name, Image, State string; Labels map[string]string; Mounts []Mount }
func (c Container) InUse() bool; func (c Container) UsesVolume(name string) bool; func (c Container) IsHelper() bool; func (c Container) ComposeService() string
type Helper struct{ Image, Volume string; ReadOnly bool; Entrypoint string; Args []string; Stdin io.Reader; Stdout io.Writer }
type HelperResult struct{ ExitCode int; Stderr string }
func BackupHelper(image, vol string, stdout io.Writer) Helper
func RestoreHelper(image, vol string, stdin io.Reader) Helper
func ClearHelper(image, vol string) Helper
func EmptyCheckHelper(image, vol string, stdout io.Writer) Helper
type Docker interface {
	Info(ctx context.Context) (HostInfo, error)
	ListVolumes(ctx context.Context) ([]Volume, error)
	VolumeSizes(ctx context.Context) (map[string]int64, error)   // absent or -1 = unknown
	InspectVolume(ctx context.Context, name string) (Volume, bool, error)
	CreateVolume(ctx context.Context, v Volume) error
	ListContainers(ctx context.Context) ([]Container, error)      // all states, helpers included
	StartedAt(ctx context.Context, id string) (time.Time, error)
	StopContainer(ctx context.Context, id string) error
	StartContainer(ctx context.Context, id string) error
	EnsureImage(ctx context.Context, ref string) (string, error)  // returns digest reference or image ID
	RunHelper(ctx context.Context, h Helper) (HelperResult, error)
	Close() error
}
type Fake struct{ ... exported fields below ... }; func NewFake() *Fake
```

- [ ] **Step 1: Write the interface and types**

`internal/dockerx/dockerx.go`:

```go
// Package dockerx wraps the Docker Engine API behind a small interface so the
// rest of dv-backup can be tested without a daemon.
package dockerx

import (
	"context"
	"io"
	"time"
)

// Container states (subset of the Engine API values) and labels used by dv-backup.
const (
	StateRunning    = "running"
	StatePaused     = "paused"
	StateRestarting = "restarting"
	StateExited     = "exited"
	StateCreated    = "created"

	LabelHelper         = "dv-backup.helper"
	LabelAnonymous      = "com.docker.volume.anonymous"
	LabelComposeProject = "com.docker.compose.project"
	LabelComposeService = "com.docker.compose.service"

	// DefaultImage is the helper image; it must contain GNU tar and findutils.
	DefaultImage = "debian:13-slim"
	// DataDir is where the volume is mounted inside helper containers.
	DataDir = "/data"
)

// HostInfo identifies the daemon.
type HostInfo struct {
	Name          string
	ServerVersion string
}

// Volume is the subset of volume metadata dv-backup needs.
type Volume struct {
	Name    string
	Driver  string
	Options map[string]string
	Labels  map[string]string
}

// Mount is one mount point of a container.
type Mount struct {
	Type        string // "volume", "bind", "tmpfs", ...
	Name        string // volume name for Type "volume"
	Source      string
	Destination string
	RW          bool
}

// Container is the subset of container metadata dv-backup needs.
type Container struct {
	ID     string
	Name   string
	Image  string
	State  string
	Labels map[string]string
	Mounts []Mount
}

// InUse reports whether the container may be writing to its volumes.
func (c Container) InUse() bool {
	switch c.State {
	case StateRunning, StatePaused, StateRestarting:
		return true
	}
	return false
}

// UsesVolume reports whether the container mounts the named volume.
func (c Container) UsesVolume(name string) bool {
	for _, m := range c.Mounts {
		if m.Type == "volume" && m.Name == name {
			return true
		}
	}
	return false
}

// IsHelper reports whether the container is a dv-backup helper.
func (c Container) IsHelper() bool { return c.Labels[LabelHelper] == "true" }

// ComposeService returns the Compose service name, or "".
func (c Container) ComposeService() string { return c.Labels[LabelComposeService] }

// Helper describes one helper container run.
type Helper struct {
	Image      string
	Volume     string
	ReadOnly   bool
	Entrypoint string
	Args       []string
	Stdin      io.Reader // nil: no stdin
	Stdout     io.Writer // nil: discard
}

// HelperResult is the outcome of a helper run that started successfully.
type HelperResult struct {
	ExitCode int
	Stderr   string
}

// BackupHelper streams a GNU tar archive of the volume to stdout.
func BackupHelper(image, vol string, stdout io.Writer) Helper {
	return Helper{Image: image, Volume: vol, ReadOnly: true, Entrypoint: "tar",
		Args: []string{"--numeric-owner", "--xattrs", "--acls", "--sparse", "-C", DataDir, "-cpf", "-", "."}, Stdout: stdout}
}

// RestoreHelper extracts a GNU tar archive from stdin into the volume.
func RestoreHelper(image, vol string, stdin io.Reader) Helper {
	return Helper{Image: image, Volume: vol, Entrypoint: "tar",
		Args: []string{"--numeric-owner", "--xattrs", "--acls", "-C", DataDir, "-xpf", "-"}, Stdin: stdin}
}

// ClearHelper deletes everything inside the volume.
func ClearHelper(image, vol string) Helper {
	return Helper{Image: image, Volume: vol, Entrypoint: "find", Args: []string{DataDir, "-mindepth", "1", "-delete"}}
}

// EmptyCheckHelper prints one path if the volume is not empty, nothing otherwise.
func EmptyCheckHelper(image, vol string, stdout io.Writer) Helper {
	return Helper{Image: image, Volume: vol, ReadOnly: true, Entrypoint: "find",
		Args: []string{DataDir, "-mindepth", "1", "-maxdepth", "1", "-print", "-quit"}, Stdout: stdout}
}

// Docker is everything dv-backup needs from the Engine API.
type Docker interface {
	Info(ctx context.Context) (HostInfo, error)
	ListVolumes(ctx context.Context) ([]Volume, error)
	// VolumeSizes returns disk usage per volume name; missing or -1 means unknown.
	VolumeSizes(ctx context.Context) (map[string]int64, error)
	InspectVolume(ctx context.Context, name string) (Volume, bool, error)
	CreateVolume(ctx context.Context, v Volume) error
	// ListContainers returns containers in every state, helpers included.
	ListContainers(ctx context.Context) ([]Container, error)
	StartedAt(ctx context.Context, id string) (time.Time, error)
	StopContainer(ctx context.Context, id string) error
	StartContainer(ctx context.Context, id string) error
	// EnsureImage pulls ref if missing and returns its digest reference (or ID).
	EnsureImage(ctx context.Context, ref string) (string, error)
	RunHelper(ctx context.Context, h Helper) (HelperResult, error)
	Close() error
}
```

- [ ] **Step 2: Write the failing fake test**

`internal/dockerx/fake_test.go`:

```go
package dockerx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestFakeHelperSemantics(t *testing.T) {
	f := NewFake()
	f.Volumes["v"] = Volume{Name: "v", Driver: "local"}
	ctx := context.Background()

	var out bytes.Buffer
	res, err := f.RunHelper(ctx, EmptyCheckHelper(DefaultImage, "v", &out))
	if err != nil || res.ExitCode != 0 || out.Len() != 0 {
		t.Fatalf("empty volume: res=%+v err=%v out=%q", res, err, out.String())
	}

	if _, err := f.RunHelper(ctx, RestoreHelper(DefaultImage, "v", strings.NewReader("TARDATA"))); err != nil {
		t.Fatal(err)
	}
	if string(f.Data["v"]) != "TARDATA" {
		t.Fatalf("restore did not store data: %q", f.Data["v"])
	}

	out.Reset()
	if _, err := f.RunHelper(ctx, EmptyCheckHelper(DefaultImage, "v", &out)); err != nil || out.Len() == 0 {
		t.Fatalf("non-empty volume should print a path")
	}

	out.Reset()
	if _, err := f.RunHelper(ctx, BackupHelper(DefaultImage, "v", &out)); err != nil || out.String() != "TARDATA" {
		t.Fatalf("backup output %q err %v", out.String(), err)
	}

	if _, err := f.RunHelper(ctx, ClearHelper(DefaultImage, "v")); err != nil || len(f.Data["v"]) != 0 {
		t.Fatal("clear did not empty the volume")
	}
	if _, err := f.RunHelper(ctx, BackupHelper(DefaultImage, "missing", &out)); err == nil {
		t.Fatal("helper on missing volume must fail")
	}
	if len(f.Calls) == 0 || !strings.HasPrefix(f.Calls[0], "helper find v") {
		t.Fatalf("calls not recorded: %v", f.Calls)
	}
}

func TestFakeContainers(t *testing.T) {
	f := NewFake()
	f.AddContainer(Container{ID: "c1", Name: "app", State: StateRunning, Mounts: []Mount{{Type: "volume", Name: "v"}}})
	ctx := context.Background()
	if err := f.StopContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	cs, _ := f.ListContainers(ctx)
	if cs[0].State != StateExited {
		t.Fatalf("state after stop = %s", cs[0].State)
	}
	if err := f.StartContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	cs, _ = f.ListContainers(ctx)
	if cs[0].State != StateRunning {
		t.Fatalf("state after start = %s", cs[0].State)
	}
	if err := f.StopContainer(ctx, "nope"); err == nil {
		t.Fatal("unknown container must fail")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/dockerx/`
Expected: FAIL, undefined `NewFake`.

- [ ] **Step 4: Write the fake**

`internal/dockerx/fake.go`:

```go
package dockerx

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Docker used by unit tests. Volume contents are modelled
// as an opaque byte slice standing in for the tar stream a helper would produce.
type Fake struct {
	mu         sync.Mutex
	Host       HostInfo
	Volumes    map[string]Volume
	Sizes      map[string]int64
	Data       map[string][]byte
	Containers map[string]*Container
	Started    map[string]time.Time
	Images     map[string]string // ref -> digest
	// Calls records every mutating or helper call, in order, e.g. "stop c1", "helper tar v".
	Calls []string
	// Failure injection.
	FailStop    map[string]error
	FailStart   map[string]error
	FailHelper  map[string]error // by "<entrypoint> <volume>", e.g. "tar app_db": RunHelper returns this error
	HelperExit  map[string]int   // by volume: backup tar exit code
	FailInfo    error
	FailInspect map[string]error
}

// NewFake returns an empty fake with a known host and the default image present.
func NewFake() *Fake {
	return &Fake{
		Host:       HostInfo{Name: "fakehost", ServerVersion: "29.8.1"},
		Volumes:    map[string]Volume{},
		Sizes:      map[string]int64{},
		Data:       map[string][]byte{},
		Containers: map[string]*Container{},
		Started:    map[string]time.Time{},
		Images:     map[string]string{DefaultImage: DefaultImage + "@sha256:fake"},
		FailStop:   map[string]error{}, FailStart: map[string]error{}, FailHelper: map[string]error{},
		HelperExit: map[string]int{}, FailInspect: map[string]error{},
	}
}

// AddVolume registers a volume with optional data.
func (f *Fake) AddVolume(v Volume, data []byte) {
	f.Volumes[v.Name] = v
	if data != nil {
		f.Data[v.Name] = data
	}
}

// AddContainer registers a container; StartedAt is derived from insertion order unless set via Started.
func (f *Fake) AddContainer(c Container) {
	cc := c
	f.Containers[c.ID] = &cc
	if _, ok := f.Started[c.ID]; !ok {
		f.Started[c.ID] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(f.Containers)) * time.Minute)
	}
}

func (f *Fake) record(format string, a ...any) { f.Calls = append(f.Calls, fmt.Sprintf(format, a...)) }

// Info implements Docker.
func (f *Fake) Info(context.Context) (HostInfo, error) { return f.Host, f.FailInfo }

// ListVolumes implements Docker (sorted by name).
func (f *Fake) ListVolumes(context.Context) ([]Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Volume, 0, len(f.Volumes))
	for _, v := range f.Volumes {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// VolumeSizes implements Docker.
func (f *Fake) VolumeSizes(context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for k, v := range f.Sizes {
		out[k] = v
	}
	return out, nil
}

// InspectVolume implements Docker.
func (f *Fake) InspectVolume(_ context.Context, name string) (Volume, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.FailInspect[name]; err != nil {
		return Volume{}, false, err
	}
	v, ok := f.Volumes[name]
	return v, ok, nil
}

// CreateVolume implements Docker.
func (f *Fake) CreateVolume(_ context.Context, v Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-volume %s", v.Name)
	if _, ok := f.Volumes[v.Name]; ok {
		return nil // Docker's create is idempotent
	}
	f.Volumes[v.Name] = v
	return nil
}

// ListContainers implements Docker (sorted by ID).
func (f *Fake) ListContainers(context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Container, 0, len(f.Containers))
	for _, c := range f.Containers {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// StartedAt implements Docker.
func (f *Fake) StartedAt(_ context.Context, id string) (time.Time, error) {
	if _, ok := f.Containers[id]; !ok {
		return time.Time{}, fmt.Errorf("no such container: %s", id)
	}
	return f.Started[id], nil
}

// StopContainer implements Docker.
func (f *Fake) StopContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop %s", id)
	if err := f.FailStop[id]; err != nil {
		return err
	}
	c, ok := f.Containers[id]
	if !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	c.State = StateExited
	return nil
}

// StartContainer implements Docker.
func (f *Fake) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start %s", id)
	if err := f.FailStart[id]; err != nil {
		return err
	}
	c, ok := f.Containers[id]
	if !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	c.State = StateRunning
	return nil
}

// EnsureImage implements Docker.
func (f *Fake) EnsureImage(_ context.Context, ref string) (string, error) {
	if d, ok := f.Images[ref]; ok {
		return d, nil
	}
	return "", fmt.Errorf("pull %s: not found (fake)", ref)
}

// RunHelper implements Docker with tar/find semantics over Data.
func (f *Fake) RunHelper(ctx context.Context, h Helper) (HelperResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("helper %s %s", h.Entrypoint, h.Volume)
	if err := ctx.Err(); err != nil {
		return HelperResult{}, err
	}
	if err := f.FailHelper[h.Entrypoint+" "+h.Volume]; err != nil {
		return HelperResult{}, err
	}
	if _, ok := f.Volumes[h.Volume]; !ok {
		return HelperResult{}, fmt.Errorf("create helper container: no such volume: %s", h.Volume)
	}
	stdout := h.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	switch h.Entrypoint {
	case "tar":
		if h.Stdin != nil {
			b, err := io.ReadAll(h.Stdin)
			if err != nil {
				return HelperResult{}, err
			}
			f.Data[h.Volume] = b
			return HelperResult{}, nil
		}
		if _, err := stdout.Write(f.Data[h.Volume]); err != nil {
			return HelperResult{}, err
		}
		if code := f.HelperExit[h.Volume]; code != 0 {
			return HelperResult{ExitCode: code, Stderr: "tar: ./x: file changed as we read it"}, nil
		}
		return HelperResult{}, nil
	case "find":
		if strings.Contains(strings.Join(h.Args, " "), "-delete") {
			delete(f.Data, h.Volume)
			return HelperResult{}, nil
		}
		if len(f.Data[h.Volume]) > 0 {
			_, _ = io.WriteString(stdout, DataDir+"/x\n")
		}
		return HelperResult{}, nil
	}
	return HelperResult{}, fmt.Errorf("fake: unsupported entrypoint %q", h.Entrypoint)
}

// Close implements Docker.
func (f *Fake) Close() error { return nil }

var _ Docker = (*Fake)(nil)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go vet ./... && go test ./internal/dockerx/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dockerx
git commit -m "Add dockerx interface, helper specs and in-memory fake"
```

---

### Task 7: Real Docker client

**Files:**
- Create: `internal/dockerx/client.go`

**Interfaces:**
- Consumes: `Docker` interface from Task 6.
- Produces: `type Client struct`; `func Connect() (*Client, error)`; `Client` implements every `Docker` method except `RunHelper`, which Task 8 adds. Until Task 8 lands, add a temporary `RunHelper` stub returning `errors.New("not implemented")` so the compile-time assertion holds.

No unit test: everything here is a thin wrapper and is exercised by the integration tests in Task 16. Verify with `go vet` and a manual smoke run at the end.

- [ ] **Step 1: Write the client**

`internal/dockerx/client.go`:

```go
package dockerx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// Client implements Docker on top of the moby client.
type Client struct {
	c *client.Client
}

// Connect creates a client from the environment (DOCKER_HOST etc.).
// API version negotiation happens on the first request.
func Connect() (*Client, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{c: c}, nil
}

// Close implements Docker.
func (d *Client) Close() error { return d.c.Close() }

// Info implements Docker.
func (d *Client) Info(ctx context.Context) (HostInfo, error) {
	info, err := d.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return HostInfo{}, fmt.Errorf("cannot reach Docker (is the daemon running and DOCKER_HOST correct?): %w", err)
	}
	ver, err := d.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return HostInfo{}, fmt.Errorf("docker version: %w", err)
	}
	return HostInfo{Name: info.Info.Name, ServerVersion: ver.Version}, nil
}

func fromVolume(v volume.Volume) Volume {
	return Volume{Name: v.Name, Driver: v.Driver, Options: v.Options, Labels: v.Labels}
}

// ListVolumes implements Docker.
func (d *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	res, err := d.c.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, fromVolume(v))
	}
	return out, nil
}

// VolumeSizes implements Docker via the disk-usage endpoint.
func (d *Client) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	res, err := d.c.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true})
	if err != nil {
		return nil, fmt.Errorf("disk usage: %w", err)
	}
	sizes := make(map[string]int64, len(res.Volumes.Items))
	for _, v := range res.Volumes.Items {
		if v.UsageData != nil {
			sizes[v.Name] = v.UsageData.Size
		}
	}
	return sizes, nil
}

// InspectVolume implements Docker.
func (d *Client) InspectVolume(ctx context.Context, name string) (Volume, bool, error) {
	res, err := d.c.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return Volume{}, false, nil
	}
	if err != nil {
		return Volume{}, false, fmt.Errorf("inspect volume %s: %w", name, err)
	}
	return fromVolume(res.Volume), true, nil
}

// CreateVolume implements Docker.
func (d *Client) CreateVolume(ctx context.Context, v Volume) error {
	_, err := d.c.VolumeCreate(ctx, client.VolumeCreateOptions{Name: v.Name, Driver: v.Driver, DriverOpts: v.Options, Labels: v.Labels})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", v.Name, err)
	}
	return nil
}

// ListContainers implements Docker.
func (d *Client) ListContainers(ctx context.Context) ([]Container, error) {
	res, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		name := s.ID[:min(12, len(s.ID))]
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		mounts := make([]Mount, 0, len(s.Mounts))
		for _, m := range s.Mounts {
			mounts = append(mounts, Mount{Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
		}
		out = append(out, Container{ID: s.ID, Name: name, Image: s.Image, State: string(s.State), Labels: s.Labels, Mounts: mounts})
	}
	return out, nil
}

// StartedAt implements Docker. A container that never started yields the zero time.
func (d *Client) StartedAt(ctx context.Context, id string) (time.Time, error) {
	res, err := d.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return time.Time{}, fmt.Errorf("inspect container %s: %w", id, err)
	}
	if res.Container.State == nil {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, res.Container.State.StartedAt)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// StopContainer implements Docker using the container's own stop timeout.
func (d *Client) StopContainer(ctx context.Context, id string) error {
	if _, err := d.c.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil {
		return err
	}
	return nil
}

// StartContainer implements Docker.
func (d *Client) StartContainer(ctx context.Context, id string) error {
	if _, err := d.c.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return err
	}
	return nil
}

// EnsureImage implements Docker. Pulls are anonymous.
func (d *Client) EnsureImage(ctx context.Context, ref string) (string, error) {
	insp, err := d.c.ImageInspect(ctx, ref)
	if cerrdefs.IsNotFound(err) {
		resp, perr := d.c.ImagePull(ctx, ref, client.ImagePullOptions{})
		if perr != nil {
			return "", pullError(ref, perr)
		}
		werr := resp.Wait(ctx)
		resp.Close()
		if werr != nil {
			return "", pullError(ref, werr)
		}
		insp, err = d.c.ImageInspect(ctx, ref)
	}
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", ref, err)
	}
	if len(insp.RepoDigests) > 0 {
		return insp.RepoDigests[0], nil
	}
	return insp.ID, nil
}

func pullError(ref string, err error) error {
	return fmt.Errorf("pull helper image %s: %w (dv-backup pulls anonymously; for a private registry run `docker pull %s` first)", ref, err, ref)
}

// RunHelper is implemented in helper.go.
var _ Docker = (*Client)(nil)

var errNotImplemented = errors.New("not implemented")
```

Until Task 8 exists, append this stub at the end of `client.go` and delete it in Task 8:

```go
// RunHelper is replaced in Task 8.
func (d *Client) RunHelper(context.Context, Helper) (HelperResult, error) { return HelperResult{}, errNotImplemented }
```

- [ ] **Step 2: Compile and vet**

Run: `go vet ./... && go build ./...`
Expected: no output.

- [ ] **Step 3: Smoke test against the local daemon**

Create a throwaway `internal/dockerx/smoke_test.go` with build tag `integration`:

```go
//go:build integration

package dockerx

import (
	"context"
	"testing"
)

func TestClientSmoke(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	info, err := d.Info(ctx)
	if err != nil || info.Name == "" || info.ServerVersion == "" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if _, err := d.ListVolumes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListContainers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.InspectVolume(ctx, "dv-backup-test-does-not-exist"); err != nil || ok {
		t.Fatalf("missing volume: ok=%v err=%v", ok, err)
	}
	digest, err := d.EnsureImage(ctx, DefaultImage)
	if err != nil || digest == "" {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	t.Log("host", info.Name, "image", digest)
}
```

Run: `go test -tags integration ./internal/dockerx/ -run Smoke -v`
Expected: PASS and a log line with the hostname and a `debian:13-slim@sha256:` digest. Keep this file; it is cheap and read-only.

- [ ] **Step 4: Commit**

```bash
git add internal/dockerx
git commit -m "Add real Docker client"
```

---
### Task 8: Helper container runner

**Files:**
- Create: `internal/dockerx/helper.go`
- Modify: `internal/dockerx/client.go` (delete the `RunHelper` stub and `errNotImplemented`)
- Modify: `internal/dockerx/smoke_test.go` (add helper round trip)

**Interfaces:**
- Produces: `func (d *Client) RunHelper(ctx context.Context, h Helper) (HelperResult, error)`. Returns an error only when the helper could not be created, started or its output could not be read. A non-zero tar exit is a normal `HelperResult`.

- [ ] **Step 1: Add the failing integration test**

Append to `internal/dockerx/smoke_test.go`:

```go
func TestHelperRoundTrip(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	name := "dv-backup-test-helper-" + randomHex(4)
	if err := d.CreateVolume(ctx, Volume{Name: name, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.c.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	var out bytes.Buffer
	res, err := d.RunHelper(ctx, EmptyCheckHelper(DefaultImage, name, &out))
	if err != nil || res.ExitCode != 0 || out.Len() != 0 {
		t.Fatalf("fresh volume not empty: res=%+v err=%v out=%q", res, err, out.String())
	}

	// Build a tar stream in Go and restore it through the helper.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "./hello.txt", Mode: 0o640, Uid: 1234, Gid: 1234, Size: 5, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	res, err = d.RunHelper(ctx, RestoreHelper(DefaultImage, name, &tarBuf))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("restore: res=%+v err=%v", res, err)
	}

	out.Reset()
	res, err = d.RunHelper(ctx, BackupHelper(DefaultImage, name, &out))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("backup: res=%+v err=%v", res, err)
	}
	tr := tar.NewReader(&out)
	found := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == "./hello.txt" {
			found = true
			if hdr.Uid != 1234 || hdr.Mode&0o777 != 0o640 {
				t.Fatalf("metadata lost: %+v", hdr)
			}
		}
	}
	if !found {
		t.Fatal("hello.txt missing from backup stream")
	}

	// Exit code and stderr surface for a failing command.
	res, err = d.RunHelper(ctx, Helper{Image: DefaultImage, Volume: name, Entrypoint: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err != nil || res.ExitCode != 3 || !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("exit code path: res=%+v err=%v", res, err)
	}

	// No helper containers left behind.
	cs, _ := d.ListContainers(ctx)
	for _, c := range cs {
		if c.IsHelper() && strings.Contains(c.Name, "dv-backup-helper-") {
			t.Fatalf("stray helper %s", c.Name)
		}
	}
}
```

The import block of `smoke_test.go` becomes `"archive/tar"`, `"bytes"`, `"context"`, `"strings"`, `"testing"` and `"github.com/moby/moby/client"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./internal/dockerx/ -run HelperRoundTrip -v`
Expected: FAIL with `not implemented` (or undefined `randomHex`).

- [ ] **Step 3: Write the runner**

`internal/dockerx/helper.go`:

```go
package dockerx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		t.buf.Next(t.buf.Len() - t.max)
	}
	return len(p), nil
}

// RunHelper runs one helper container to completion. See spec §11.2 for the
// ordering rules: attach and wait are set up before start so nothing is lost;
// the container is removed explicitly with a non-cancelled context.
func (d *Client) RunHelper(ctx context.Context, h Helper) (HelperResult, error) {
	cfg := &container.Config{
		Image:        h.Image,
		Entrypoint:   []string{h.Entrypoint},
		Cmd:          h.Args,
		User:         "0:0",
		Tty:          false,
		AttachStdout: true,
		AttachStderr: true,
		Labels:       map[string]string{LabelHelper: "true"},
	}
	if h.Stdin != nil {
		cfg.OpenStdin, cfg.StdinOnce, cfg.AttachStdin = true, true, true
	}
	host := &container.HostConfig{
		NetworkMode: "none",
		Mounts: []mount.Mount{{
			Type:          mount.TypeVolume,
			Source:        h.Volume,
			Target:        DataDir,
			ReadOnly:      h.ReadOnly,
			VolumeOptions: &mount.VolumeOptions{NoCopy: true},
		}},
	}
	created, err := d.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: host, Name: "dv-backup-helper-" + randomHex(6),
	})
	if err != nil {
		return HelperResult{}, fmt.Errorf("create helper container: %w", err)
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = d.c.ContainerRemove(rmCtx, created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	attach, err := d.c.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true, Stdin: h.Stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		return HelperResult{}, fmt.Errorf("attach to helper container: %w", err)
	}
	defer attach.Close()

	wait := d.c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	if _, err := d.c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return HelperResult{}, fmt.Errorf("start helper container: %w", err)
	}

	stderr := &tailBuffer{max: 64 * 1024}
	stdout := h.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, attach.Reader)
		outDone <- err
	}()

	var sendErr error
	if h.Stdin != nil {
		if _, err := io.Copy(attach.Conn, h.Stdin); err != nil {
			sendErr = err // tar may have exited early; report after we know its exit code
		}
		_ = attach.CloseWrite()
	}

	// Either the output stream ends (container exited) or reading it fails
	// (e.g. our consumer stopped); in the latter case return so the deferred
	// force-remove kills the helper instead of letting it block on stdout.
	var outErr error
	outClosed := false
	select {
	case outErr = <-outDone:
		outClosed = true
	case res := <-wait.Result:
		return d.finish(res, stderr, sendErr, outDone)
	case err := <-wait.Error:
		return HelperResult{}, fmt.Errorf("wait for helper container: %w", err)
	}
	if outClosed && outErr != nil && !errors.Is(outErr, io.EOF) {
		return HelperResult{}, fmt.Errorf("read helper output: %w", outErr)
	}
	select {
	case res := <-wait.Result:
		return d.finish(res, stderr, sendErr, nil)
	case err := <-wait.Error:
		return HelperResult{}, fmt.Errorf("wait for helper container: %w", err)
	}
}

func (d *Client) finish(res container.WaitResponse, stderr *tailBuffer, sendErr error, outDone <-chan error) (HelperResult, error) {
	if outDone != nil {
		if err := <-outDone; err != nil && !errors.Is(err, io.EOF) {
			return HelperResult{}, fmt.Errorf("read helper output: %w", err)
		}
	}
	if res.Error != nil {
		return HelperResult{}, fmt.Errorf("helper container: %s", res.Error.Message)
	}
	r := HelperResult{ExitCode: int(res.StatusCode), Stderr: stderr.buf.String()}
	if sendErr != nil && r.ExitCode == 0 {
		return r, fmt.Errorf("send data to helper container: %w", sendErr)
	}
	return r, nil
}
```

Remove the stub and `errNotImplemented` from `client.go`, and its `"errors"` import if now unused.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./... && go test -tags integration ./internal/dockerx/ -v`
Expected: PASS for both tests. If `TestHelperRoundTrip` hangs at the restore step, the attach connection did not deliver EOF; check that `CloseWrite` is called and that `StdinOnce` is set.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerx
git commit -m "Run helper containers over the Docker API"
```

---

### Task 9: Volume selection

**Files:**
- Create: `internal/selection/selection.go`, `internal/selection/selection_test.go`

**Interfaces:**
- Produces:
  - `func IsAnonymous(v dockerx.Volume) bool`
  - `func Named(vols []dockerx.Volume) []dockerx.Volume` (drops anonymous, sorts by name)
  - `func Filter[T any](items []T, name func(T) string, project func(T) string, names, projects []string) ([]T, error)` (union of name and project filters; no filters = everything; unknown name = error; empty result = error)
  - `func Volumes(vols []dockerx.Volume, names, projects []string) ([]dockerx.Volume, error)` (= `Named` + `Filter`)

- [ ] **Step 1: Write the failing tests**

```go
package selection

import (
	"strings"
	"testing"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

func vol(name, project string) dockerx.Volume {
	v := dockerx.Volume{Name: name, Driver: "local", Labels: map[string]string{}}
	if project != "" {
		v.Labels[dockerx.LabelComposeProject] = project
	}
	return v
}

func names(vs []dockerx.Volume) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return strings.Join(out, ",")
}

func TestIsAnonymous(t *testing.T) {
	labelled := dockerx.Volume{Name: "named", Labels: map[string]string{dockerx.LabelAnonymous: ""}}
	hexName := dockerx.Volume{Name: strings.Repeat("ab", 32)}
	if !IsAnonymous(labelled) || !IsAnonymous(hexName) {
		t.Fatal("anonymous volumes not detected")
	}
	if IsAnonymous(vol("app_data", "app")) || IsAnonymous(dockerx.Volume{Name: strings.Repeat("ab", 31)}) {
		t.Fatal("named volume classified anonymous")
	}
}

func TestVolumesFilters(t *testing.T) {
	all := []dockerx.Volume{vol("z_db", "z"), vol("a_data", "a"), vol("a_cache", "a"), vol("plain", ""),
		{Name: strings.Repeat("0", 64), Labels: map[string]string{dockerx.LabelAnonymous: ""}}}

	got, err := Volumes(all, nil, nil)
	if err != nil || names(got) != "a_cache,a_data,plain,z_db" {
		t.Fatalf("no filter: %s %v", names(got), err)
	}
	got, err = Volumes(all, []string{"plain"}, []string{"a"})
	if err != nil || names(got) != "a_cache,a_data,plain" {
		t.Fatalf("union: %s %v", names(got), err)
	}
	got, err = Volumes(all, []string{"z_db", "z_db"}, nil)
	if err != nil || names(got) != "z_db" {
		t.Fatalf("dedupe: %s %v", names(got), err)
	}
	if _, err = Volumes(all, []string{"nope"}, nil); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown name: %v", err)
	}
	if _, err = Volumes(all, nil, []string{"ghost"}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err = Volumes(all, []string{strings.Repeat("0", 64)}, nil); err == nil {
		t.Fatal("anonymous volume must not be selectable")
	}
	if _, err = Volumes(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "no named volumes") {
		t.Fatalf("empty host: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/selection/`
Expected: FAIL, undefined `Volumes`.

- [ ] **Step 3: Write the implementation**

```go
// Package selection filters volumes by name and Compose project.
package selection

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsAnonymous reports whether a volume was created anonymously by Docker.
func IsAnonymous(v dockerx.Volume) bool {
	if _, ok := v.Labels[dockerx.LabelAnonymous]; ok {
		return true
	}
	return hex64.MatchString(v.Name)
}

// Named returns the non-anonymous volumes sorted by name.
func Named(vols []dockerx.Volume) []dockerx.Volume {
	out := make([]dockerx.Volume, 0, len(vols))
	for _, v := range vols {
		if !IsAnonymous(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Filter applies the union of explicit names and Compose projects. With no
// filters every item is returned. A name or project that matches nothing is
// an error, as is an empty result.
func Filter[T any](items []T, name func(T) string, project func(T) string, names, projects []string) ([]T, error) {
	if len(names) == 0 && len(projects) == 0 {
		if len(items) == 0 {
			return nil, fmt.Errorf("no named volumes found")
		}
		return items, nil
	}
	wantName := map[string]bool{}
	for _, n := range names {
		wantName[n] = false
	}
	wantProject := map[string]bool{}
	for _, p := range projects {
		wantProject[p] = false
	}
	var out []T
	for _, it := range items {
		_, byName := wantName[name(it)]
		_, byProject := wantProject[project(it)]
		if byName {
			wantName[name(it)] = true
		}
		if byProject {
			wantProject[project(it)] = true
		}
		if byName || byProject {
			out = append(out, it)
		}
	}
	var missing []string
	for n, seen := range wantName {
		if !seen {
			missing = append(missing, "volume "+n)
		}
	}
	for p, seen := range wantProject {
		if !seen {
			missing = append(missing, "project "+p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("not found: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Volumes selects named host volumes by names and projects.
func Volumes(vols []dockerx.Volume, names, projects []string) ([]dockerx.Volume, error) {
	return Filter(Named(vols),
		func(v dockerx.Volume) string { return v.Name },
		func(v dockerx.Volume) string { return v.Labels[dockerx.LabelComposeProject] },
		names, projects)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/selection/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/selection
git commit -m "Add volume selection by name and Compose project"
```

---

### Task 10: Stopper (stop bookkeeping and ordered restart)

**Files:**
- Create: `internal/stopper/stopper.go`, `internal/stopper/stopper_test.go`

**Interfaces:**
- Consumes: `dockerx.Docker`, `dockerx.Container`.
- Produces: `type Stopper`; `func New(d dockerx.Docker, out io.Writer) *Stopper`; `(*Stopper).Stop(ctx, containers []dockerx.Container) error` (skips containers not in use, dedupes by ID, records `StartedAt` before stopping, stops in the given order and stops at the first error); `(*Stopper).Stopped() []dockerx.Container`; `(*Stopper).Restart(ctx) []error` (ascending `StartedAt`, continues past failures, returns one error per failed start, clears its list). `func UsersOf(volumes []string, containers []dockerx.Container) []dockerx.Container` (in-use, non-helper containers mounting any of the volumes, deduped, sorted by name).

- [ ] **Step 1: Write the failing tests**

```go
package stopper

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

func c(id, state string, vols ...string) dockerx.Container {
	cc := dockerx.Container{ID: id, Name: "name-" + id, State: state}
	for _, v := range vols {
		cc.Mounts = append(cc.Mounts, dockerx.Mount{Type: "volume", Name: v})
	}
	return cc
}

func TestUsersOf(t *testing.T) {
	helper := c("h", dockerx.StateRunning, "v1")
	helper.Labels = map[string]string{dockerx.LabelHelper: "true"}
	cs := []dockerx.Container{c("b", dockerx.StateRunning, "v1", "v2"), c("a", dockerx.StatePaused, "v2"),
		c("x", dockerx.StateExited, "v1"), c("r", dockerx.StateRestarting, "v3"), c("o", dockerx.StateRunning, "other"), helper}
	got := UsersOf([]string{"v1", "v2"}, cs)
	var ids []string
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("users = %v", ids)
	}
}

func TestStopAndRestartOrder(t *testing.T) {
	f := dockerx.NewFake()
	for _, cc := range []dockerx.Container{c("late", dockerx.StateRunning, "v"), c("early", dockerx.StatePaused, "v"), c("idle", dockerx.StateExited, "v")} {
		f.AddContainer(cc)
	}
	f.Started["early"] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Started["late"] = time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	s := New(f, &out)
	cs, _ := f.ListContainers(context.Background())
	if err := s.Stop(context.Background(), append(cs, cs[0])); err != nil {
		t.Fatal(err)
	}
	if len(s.Stopped()) != 2 {
		t.Fatalf("stopped %d, want 2 (idle skipped, duplicate ignored)", len(s.Stopped()))
	}
	if errs := s.Restart(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if strings.Join(f.Calls, " ") != "stop early stop late start early start late" {
		t.Fatalf("calls: %v", f.Calls)
	}
	if len(s.Stopped()) != 0 {
		t.Fatal("Restart must clear the list")
	}
	if !strings.Contains(out.String(), "stopping container name-early (paused)") || !strings.Contains(out.String(), "starting container name-late") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestStopErrorKeepsAlreadyStopped(t *testing.T) {
	f := dockerx.NewFake()
	f.AddContainer(c("a", dockerx.StateRunning, "v"))
	f.AddContainer(c("b", dockerx.StateRunning, "v"))
	f.FailStop["b"] = errors.New("boom")
	s := New(f, &bytes.Buffer{})
	cs, _ := f.ListContainers(context.Background())
	err := s.Stop(context.Background(), cs)
	if err == nil || !strings.Contains(err.Error(), "name-b") {
		t.Fatalf("err = %v", err)
	}
	if len(s.Stopped()) != 1 || s.Stopped()[0].ID != "a" {
		t.Fatalf("stopped = %+v", s.Stopped())
	}
}

func TestRestartReportsEveryFailure(t *testing.T) {
	f := dockerx.NewFake()
	f.AddContainer(c("a", dockerx.StateRunning, "v"))
	f.AddContainer(c("b", dockerx.StateRunning, "v"))
	f.FailStart["a"] = errors.New("boom")
	s := New(f, &bytes.Buffer{})
	cs, _ := f.ListContainers(context.Background())
	_ = s.Stop(context.Background(), cs)
	errs := s.Restart(context.Background())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "name-a") {
		t.Fatalf("errs = %v", errs)
	}
	if strings.Join(f.Calls, " ") != "stop a stop b start a start b" {
		t.Fatalf("calls: %v (b must still be started)", f.Calls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/stopper/`
Expected: FAIL, undefined `New`.

- [ ] **Step 3: Write the implementation**

```go
// Package stopper stops containers and restarts exactly those it stopped.
package stopper

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

type entry struct {
	c         dockerx.Container
	startedAt time.Time
}

// Stopper remembers which containers it stopped.
type Stopper struct {
	d       dockerx.Docker
	out     io.Writer
	stopped []entry
}

// New returns a Stopper writing progress lines to out.
func New(d dockerx.Docker, out io.Writer) *Stopper { return &Stopper{d: d, out: out} }

// UsersOf returns in-use, non-helper containers that mount any of the volumes,
// deduplicated and sorted by name.
func UsersOf(volumes []string, containers []dockerx.Container) []dockerx.Container {
	seen := map[string]bool{}
	var out []dockerx.Container
	for _, c := range containers {
		if !c.InUse() || c.IsHelper() || seen[c.ID] {
			continue
		}
		for _, v := range volumes {
			if c.UsesVolume(v) {
				seen[c.ID] = true
				out = append(out, c)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Stop stops every in-use container once, in the given order, and records it.
// It returns at the first failure; containers stopped so far stay recorded.
func (s *Stopper) Stop(ctx context.Context, containers []dockerx.Container) error {
	done := map[string]bool{}
	for _, e := range s.stopped {
		done[e.c.ID] = true
	}
	for _, c := range containers {
		if !c.InUse() || done[c.ID] {
			continue
		}
		startedAt, err := s.d.StartedAt(ctx, c.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(s.out, "stopping container %s (%s)\n", c.Name, c.State)
		if err := s.d.StopContainer(ctx, c.ID); err != nil {
			return fmt.Errorf("stop container %s: %w", c.Name, err)
		}
		done[c.ID] = true
		s.stopped = append(s.stopped, entry{c: c, startedAt: startedAt})
	}
	return nil
}

// Stopped lists the containers stopped so far.
func (s *Stopper) Stopped() []dockerx.Container {
	out := make([]dockerx.Container, 0, len(s.stopped))
	for _, e := range s.stopped {
		out = append(out, e.c)
	}
	return out
}

// Restart starts the stopped containers in ascending order of their original
// start time. Every failure is reported; the list is cleared afterwards.
// Callers pass a context that is not cancelled (context.WithoutCancel).
func (s *Stopper) Restart(ctx context.Context) []error {
	sort.SliceStable(s.stopped, func(i, j int) bool { return s.stopped[i].startedAt.Before(s.stopped[j].startedAt) })
	var errs []error
	for _, e := range s.stopped {
		fmt.Fprintf(s.out, "starting container %s\n", e.c.Name)
		if err := s.d.StartContainer(ctx, e.c.ID); err != nil {
			errs = append(errs, fmt.Errorf("start container %s: %w", e.c.Name, err))
		}
	}
	s.stopped = nil
	return errs
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/stopper/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stopper
git commit -m "Add container stop bookkeeping with ordered restart"
```

---
### Task 11: Backup orchestration

**Files:**
- Create: `internal/backup/backup.go`, `internal/backup/backup_test.go`

**Interfaces:**
- Consumes: `archive.NewWriter/AddVolume/Finish/Abort`, `manifest.*`, `selection.Volumes`, `stopper.New/UsersOf/Stop/Restart`, `dockerx.BackupHelper`, `version.Tool()`.
- Produces:
  - `type Options struct { OutputDir string; Names, Projects []string; NoStop bool; Image string; Now func() time.Time }`
  - `type Result struct { Path string; RestartErrors []error }`
  - `func Run(ctx context.Context, d dockerx.Docker, out, errOut io.Writer, opts Options) (Result, error)` — on error the `.partial` is removed and containers are restarted anyway; `Result.RestartErrors` is populated in both cases.

- [ ] **Step 1: Write the failing tests**

```go
package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

var now = func() time.Time { return time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC) }

func setup(t *testing.T) (*dockerx.Fake, Options) {
	t.Helper()
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local", Labels: map[string]string{dockerx.LabelComposeProject: "app"}}, []byte("DBDATA"))
	f.AddVolume(dockerx.Volume{Name: "app_files", Driver: "local", Labels: map[string]string{dockerx.LabelComposeProject: "app"}}, []byte("FILES"))
	f.AddVolume(dockerx.Volume{Name: "other", Driver: "local"}, []byte("OTHER"))
	f.AddContainer(dockerx.Container{ID: "db", Name: "app-db-1", Image: "postgres:17", State: dockerx.StateRunning,
		Labels: map[string]string{dockerx.LabelComposeService: "db"}, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	f.AddContainer(dockerx.Container{ID: "web", Name: "app-web-1", Image: "nginx", State: dockerx.StateExited,
		Mounts: []dockerx.Mount{{Type: "volume", Name: "app_files"}}})
	f.Sizes["app_db"] = 6
	return f, Options{OutputDir: t.TempDir(), Image: dockerx.DefaultImage, Now: now}
}

func TestBackupAllVolumes(t *testing.T) {
	f, opts := setup(t)
	var out, errOut bytes.Buffer
	res, err := Run(context.Background(), f, &out, &errOut, opts)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res.Path) != "dv-backup-fakehost-20260917T120000Z.tar" {
		t.Fatalf("path %s", res.Path)
	}
	r, err := archive.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := r.Manifest()
	if len(m.Volumes) != 3 || m.Host != "fakehost" || m.DockerVersion != "29.8.1" || m.HelperImage != dockerx.DefaultImage+"@sha256:fake" || !strings.HasPrefix(m.Tool, "dv-backup ") {
		t.Fatalf("manifest %+v", m)
	}
	db, _ := m.Find("app_db")
	if db.SizeBytes != 6 || !db.Consistent || db.Project() != "app" || len(db.Containers) != 1 || db.Containers[0].Name != "app-db-1" || !db.Containers[0].WasRunning || db.Containers[0].ComposeService != "db" {
		t.Fatalf("app_db entry %+v", db)
	}
	files, _ := m.Find("app_files")
	if len(files.Containers) != 1 || files.Containers[0].WasRunning {
		t.Fatalf("app_files containers %+v", files.Containers)
	}
	rc, _ := r.OpenVolume("app_db")
	data := make([]byte, 16)
	n, _ := rc.Read(data)
	if string(data[:n]) != "DBDATA" {
		t.Fatalf("volume data %q", data[:n])
	}
	if strings.Join(f.Calls, " ") != "stop db helper tar app_db helper tar app_files helper tar other start db" {
		t.Fatalf("calls %v", f.Calls)
	}
	for _, want := range []string{"backing up volume app_db", "wrote " + res.Path} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

func TestBackupFilters(t *testing.T) {
	f, opts := setup(t)
	opts.Names = []string{"other"}
	res, err := Run(context.Background(), f, &bytes.Buffer{}, &bytes.Buffer{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := archive.Open(res.Path)
	defer r.Close()
	if len(r.Manifest().Volumes) != 1 || r.Manifest().Volumes[0].Name != "other" {
		t.Fatalf("volumes %+v", r.Manifest().Volumes)
	}
	if strings.Contains(strings.Join(f.Calls, " "), "stop") {
		t.Fatalf("no container uses 'other', nothing should be stopped: %v", f.Calls)
	}

	opts.Names = []string{"missing"}
	if _, err := Run(context.Background(), f, &bytes.Buffer{}, &bytes.Buffer{}, opts); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown name: %v", err)
	}
	entries, _ := os.ReadDir(opts.OutputDir)
	if len(entries) != 1 {
		t.Fatalf("a failed selection must not leave files: %v", entries)
	}
}

func TestBackupFailureRestartsAndCleansUp(t *testing.T) {
	f, opts := setup(t)
	f.FailHelper["tar app_files"] = errors.New("tar exploded")
	var out, errOut bytes.Buffer
	res, err := Run(context.Background(), f, &out, &errOut, opts)
	if err == nil || !strings.Contains(err.Error(), "tar exploded") {
		t.Fatalf("err = %v", err)
	}
	if len(res.RestartErrors) != 0 {
		t.Fatal(res.RestartErrors)
	}
	cs, _ := f.ListContainers(context.Background())
	for _, c := range cs {
		if c.ID == "db" && c.State != dockerx.StateRunning {
			t.Fatal("db container not restarted")
		}
	}
	entries, _ := os.ReadDir(opts.OutputDir)
	if len(entries) != 0 {
		t.Fatalf("output dir not clean: %v", entries)
	}
}

func TestBackupTarExitCodes(t *testing.T) {
	f, opts := setup(t)
	f.HelperExit["app_db"] = 1
	if _, err := Run(context.Background(), f, &bytes.Buffer{}, &bytes.Buffer{}, opts); err == nil || !strings.Contains(err.Error(), "exit") {
		t.Fatalf("stopped backup with tar exit 1 must fail: %v", err)
	}

	f, opts = setup(t)
	f.HelperExit["app_db"] = 1
	opts.NoStop = true
	var errOut bytes.Buffer
	res, err := Run(context.Background(), f, &bytes.Buffer{}, &errOut, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := archive.Open(res.Path)
	defer r.Close()
	db, _ := r.Manifest().Find("app_db")
	if db.Consistent {
		t.Fatal("app_db must be marked inconsistent")
	}
	other, _ := r.Manifest().Find("other")
	if !other.Consistent {
		t.Fatal("'other' has no running user and tar exit 0; must stay consistent")
	}
	if !strings.Contains(errOut.String(), "warning") {
		t.Fatalf("expected warning on stderr: %s", errOut.String())
	}
	if strings.Contains(strings.Join(f.Calls, " "), "stop") {
		t.Fatal("--no-stop must not stop containers")
	}

	f, opts = setup(t)
	f.HelperExit["app_db"] = 2
	opts.NoStop = true
	if _, err := Run(context.Background(), f, &bytes.Buffer{}, &bytes.Buffer{}, opts); err == nil {
		t.Fatal("tar exit 2 must fail even with --no-stop")
	}
}

func TestBackupRestartFailureIsReported(t *testing.T) {
	f, opts := setup(t)
	f.FailStart["db"] = errors.New("cannot start")
	res, err := Run(context.Background(), f, &bytes.Buffer{}, &bytes.Buffer{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RestartErrors) != 1 || res.Path == "" {
		t.Fatalf("res %+v", res)
	}
}

func TestBackupCancelledContextStillRestarts(t *testing.T) {
	f, opts := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, f, &bytes.Buffer{}, &bytes.Buffer{}, opts)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	calls := strings.Join(f.Calls, " ")
	if strings.Contains(calls, "stop db") && !strings.Contains(calls, "start db") {
		t.Fatalf("stopped but not restarted: %v", f.Calls)
	}
	entries, _ := os.ReadDir(opts.OutputDir)
	if len(entries) != 0 {
		t.Fatalf("output dir not clean: %v", entries)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/backup/`
Expected: FAIL, undefined `Run`.

- [ ] **Step 3: Write the implementation**

```go
// Package backup archives selected volumes into one archive file.
package backup

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
	"github.com/alexander-jacob/dv-backup/internal/selection"
	"github.com/alexander-jacob/dv-backup/internal/stopper"
	"github.com/alexander-jacob/dv-backup/internal/version"
)

// Options controls a backup run.
type Options struct {
	OutputDir string
	Names     []string
	Projects  []string
	NoStop    bool
	Image     string
	Now       func() time.Time // nil = time.Now
}

// Result reports the archive written and any containers that failed to restart.
type Result struct {
	Path          string
	RestartErrors []error
}

// Run performs a backup. Containers stopped by Run are always restarted, even
// on error or cancellation; the archive is removed on error.
func Run(ctx context.Context, d dockerx.Docker, out, errOut io.Writer, opts Options) (res Result, err error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// Preflight: everything that can fail before any state changes.
	info, err := d.Info(ctx)
	if err != nil {
		return res, err
	}
	digest, err := d.EnsureImage(ctx, opts.Image)
	if err != nil {
		return res, err
	}
	vols, err := d.ListVolumes(ctx)
	if err != nil {
		return res, err
	}
	selected, err := selection.Volumes(vols, opts.Names, opts.Projects)
	if err != nil {
		return res, err
	}
	containers, err := d.ListContainers(ctx)
	if err != nil {
		return res, err
	}
	if sizes, err := d.VolumeSizes(ctx); err == nil {
		warnFreeSpace(errOut, opts.OutputDir, selected, sizes)
	}
	w, err := archive.NewWriter(opts.OutputDir, info.Name, now())
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			if aerr := w.Abort(); aerr != nil {
				fmt.Fprintf(errOut, "warning: %v\n", aerr)
			}
		}
	}()

	names := make([]string, len(selected))
	for i, v := range selected {
		names[i] = v.Name
	}
	st := stopper.New(d, out)
	defer func() {
		res.RestartErrors = st.Restart(context.WithoutCancel(ctx))
	}()
	if !opts.NoStop {
		if err = st.Stop(ctx, stopper.UsersOf(names, containers)); err != nil {
			return res, err
		}
	}

	m := &manifest.Manifest{
		FormatVersion: manifest.FormatVersion, Tool: version.Tool(), CreatedAt: now().UTC(),
		Host: info.Name, DockerVersion: info.ServerVersion, HelperImage: digest,
	}
	for _, v := range selected {
		fmt.Fprintf(out, "backing up volume %s\n", v.Name)
		start := time.Now()
		mv, verr := backupVolume(ctx, d, w, opts, v, containers)
		if verr != nil {
			err = verr
			return res, err
		}
		if !mv.Consistent {
			fmt.Fprintf(errOut, "warning: volume %s was backed up while in use; marked inconsistent\n", v.Name)
		}
		fmt.Fprintf(out, "  %s: %s data, %s compressed, %s\n", v.Name,
			manifest.FormatBytes(mv.SizeBytes), manifest.FormatBytes(mv.Archive.SizeBytes), time.Since(start).Round(time.Millisecond))
		m.Volumes = append(m.Volumes, mv)
	}
	if err = w.Finish(m); err != nil {
		return res, err
	}
	res.Path = w.Path()
	fmt.Fprintf(out, "wrote %s\n", res.Path)
	return res, nil
}

func backupVolume(ctx context.Context, d dockerx.Docker, w *archive.Writer, opts Options, v dockerx.Volume, containers []dockerx.Container) (manifest.Volume, error) {
	users := containersUsing(v.Name, containers)
	anyInUse := false
	mv := manifest.Volume{Name: v.Name, Driver: v.Driver, DriverOpts: orEmpty(v.Options), Labels: orEmpty(v.Labels), Consistent: true}
	for _, c := range users {
		mv.Containers = append(mv.Containers, manifest.Container{Name: c.Name, Image: c.Image, ComposeService: c.ComposeService(), WasRunning: c.InUse()})
		anyInUse = anyInUse || c.InUse()
	}

	pr, pw := io.Pipe()
	type outcome struct {
		res dockerx.HelperResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := d.RunHelper(ctx, dockerx.BackupHelper(opts.Image, v.Name, pw))
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
		done <- outcome{res, err}
	}()
	entry, uncompressed, addErr := w.AddVolume(v.Name, pr)
	if addErr != nil {
		pr.CloseWithError(addErr) // unblocks the helper's writes
	}
	o := <-done
	if o.err != nil {
		return mv, fmt.Errorf("volume %s: %w", v.Name, o.err)
	}
	if addErr != nil {
		return mv, addErr
	}
	switch {
	case o.res.ExitCode == 0:
	case o.res.ExitCode == 1 && opts.NoStop:
		mv.Consistent = false
	default:
		return mv, fmt.Errorf("volume %s: tar exit %d: %s", v.Name, o.res.ExitCode, o.res.Stderr)
	}
	if opts.NoStop && anyInUse {
		mv.Consistent = false
	}
	mv.SizeBytes = uncompressed
	mv.Archive = entry
	return mv, nil
}

func containersUsing(volume string, containers []dockerx.Container) []dockerx.Container {
	var out []dockerx.Container
	for _, c := range containers {
		if !c.IsHelper() && c.UsesVolume(volume) {
			out = append(out, c)
		}
	}
	return out
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// warnFreeSpace prints a warning if the selected volumes' disk usage exceeds
// the free space in dir. Sizes are approximate (disk usage, not tar size).
func warnFreeSpace(errOut io.Writer, dir string, selected []dockerx.Volume, sizes map[string]int64) {
	var total int64
	for _, v := range selected {
		if s, ok := sizes[v.Name]; ok && s > 0 {
			total += s
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return
	}
	free := int64(st.Bavail) * st.Bsize // Bsize is int64 on linux/amd64 and linux/arm64
	if total > free {
		fmt.Fprintf(errOut, "warning: selected volumes use %s but %s has only %s free\n",
			manifest.FormatBytes(total), dir, manifest.FormatBytes(free))
	}
}

```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./... && go test ./internal/backup/`
Expected: PASS. If `TestBackupCancelledContextStillRestarts` fails because `Run` returns before creating a writer, that is fine: the assertion only requires that whatever was stopped is restarted and that no files remain.

- [ ] **Step 5: Commit**

```bash
git add internal/backup
git commit -m "Add backup orchestration"
```

---

### Task 12: Restore planner (pure)

**Files:**
- Create: `internal/restore/plan.go`, `internal/restore/plan_test.go`

**Interfaces:**
- Produces:
  - `type Action int` with `ActionCreate`, `ActionUnpack`, `ActionOverwrite`, `ActionBlocked`; `(Action).String()` returns `create`, `unpack`, `overwrite`, `blocked`.
  - `type State struct { Exists, Empty bool; Users []dockerx.Container; Existing dockerx.Volume }`
  - `type Step struct { Volume manifest.Volume; Action Action; Reason string; Stop []dockerx.Container; Warnings []string }`
  - `type Plan struct { Steps []Step }`; `(Plan).Blocked() bool`; `(Plan).ToStop() []dockerx.Container` (deduped, in step order)
  - `func BuildPlan(vols []manifest.Volume, states map[string]State, force bool) Plan`

- [ ] **Step 1: Write the failing tests**

```go
package restore

import (
	"strings"
	"testing"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

func mv(name string) manifest.Volume {
	return manifest.Volume{Name: name, Driver: "local", DriverOpts: map[string]string{}, Labels: map[string]string{"com.docker.compose.project": "app"}}
}

func running(id string) dockerx.Container {
	return dockerx.Container{ID: id, Name: "c-" + id, State: dockerx.StateRunning}
}

func TestBuildPlanTable(t *testing.T) {
	cases := []struct {
		name       string
		state      State
		force      bool
		want       Action
		stops      int
		reasonPart string
	}{
		{"missing", State{}, false, ActionCreate, 0, "does not exist"},
		{"missing force", State{}, true, ActionCreate, 0, "does not exist"},
		{"empty idle", State{Exists: true, Empty: true}, false, ActionUnpack, 0, "empty"},
		{"empty idle force", State{Exists: true, Empty: true}, true, ActionUnpack, 0, "empty"},
		{"has data", State{Exists: true}, false, ActionBlocked, 0, "contains data"},
		{"has data force", State{Exists: true}, true, ActionOverwrite, 0, "contains data"},
		{"empty in use", State{Exists: true, Empty: true, Users: []dockerx.Container{running("a")}}, false, ActionBlocked, 0, "in use by c-a (running)"},
		{"empty in use force", State{Exists: true, Empty: true, Users: []dockerx.Container{running("a")}}, true, ActionOverwrite, 1, "in use"},
		{"data in use force", State{Exists: true, Users: []dockerx.Container{running("a"), {ID: "b", Name: "c-b", State: dockerx.StatePaused}, {ID: "x", Name: "c-x", State: dockerx.StateExited}}}, true, ActionOverwrite, 2, "c-b (paused)"},
	}
	for _, tc := range cases {
		p := BuildPlan([]manifest.Volume{mv("v")}, map[string]State{"v": tc.state}, tc.force)
		s := p.Steps[0]
		if s.Action != tc.want || len(s.Stop) != tc.stops || !strings.Contains(s.Reason, tc.reasonPart) {
			t.Errorf("%s: got %s stops=%d reason=%q", tc.name, s.Action, len(s.Stop), s.Reason)
		}
		if s.Action == ActionBlocked && !strings.Contains(s.Reason, "--force") {
			t.Errorf("%s: blocked reason must mention --force", tc.name)
		}
		if p.Blocked() != (tc.want == ActionBlocked) {
			t.Errorf("%s: Blocked() wrong", tc.name)
		}
	}
}

func TestBuildPlanWarningsAndToStop(t *testing.T) {
	existing := dockerx.Volume{Name: "v", Driver: "nfs", Options: map[string]string{"o": "addr=x"}, Labels: map[string]string{}}
	shared := running("a")
	states := map[string]State{
		"v": {Exists: true, Existing: existing, Users: []dockerx.Container{shared}},
		"w": {Exists: true, Empty: true, Users: []dockerx.Container{shared, running("b")},
			Existing: dockerx.Volume{Name: "w", Driver: "local", Options: map[string]string{}, Labels: map[string]string{"com.docker.compose.project": "app"}}},
	}
	p := BuildPlan([]manifest.Volume{mv("v"), mv("w")}, states, true)
	if len(p.Steps[0].Warnings) != 3 {
		t.Fatalf("expected driver, options and labels warnings, got %v", p.Steps[0].Warnings)
	}
	if len(p.Steps[1].Warnings) != 0 {
		t.Fatalf("no warnings expected when metadata matches, got %v", p.Steps[1].Warnings)
	}
	ids := []string{}
	for _, c := range p.ToStop() {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("ToStop = %v", ids)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/restore/`
Expected: FAIL, undefined `BuildPlan`.

- [ ] **Step 3: Write the implementation**

```go
// Package restore verifies an archive, plans changes and applies them.
package restore

import (
	"fmt"
	"maps"
	"strings"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

// Action is what the plan does with one volume.
type Action int

// Plan actions.
const (
	ActionCreate Action = iota
	ActionUnpack
	ActionOverwrite
	ActionBlocked
)

func (a Action) String() string {
	return [...]string{"create", "unpack", "overwrite", "blocked"}[a]
}

// State is what exists on the host for one volume.
type State struct {
	Exists   bool
	Empty    bool
	Users    []dockerx.Container // containers mounting the volume, any state, helpers excluded
	Existing dockerx.Volume
}

// Step is the planned action for one volume.
type Step struct {
	Volume   manifest.Volume
	Action   Action
	Reason   string
	Stop     []dockerx.Container
	Warnings []string
}

// Plan is the full set of steps in manifest order.
type Plan struct {
	Steps []Step
}

// Blocked reports whether any step is blocked.
func (p Plan) Blocked() bool {
	for _, s := range p.Steps {
		if s.Action == ActionBlocked {
			return true
		}
	}
	return false
}

// ToStop lists every container the plan stops, once each, in step order.
func (p Plan) ToStop() []dockerx.Container {
	seen := map[string]bool{}
	var out []dockerx.Container
	for _, s := range p.Steps {
		for _, c := range s.Stop {
			if !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// BuildPlan decides per volume what to do. It is a pure function of the
// manifest volumes, the observed host state and the --force flag (spec §7.3).
func BuildPlan(vols []manifest.Volume, states map[string]State, force bool) Plan {
	var p Plan
	for _, v := range vols {
		st := states[v.Name]
		step := Step{Volume: v}
		var inUse []dockerx.Container
		var users []string
		for _, c := range st.Users {
			if c.InUse() {
				inUse = append(inUse, c)
				users = append(users, fmt.Sprintf("%s (%s)", c.Name, c.State))
			}
		}
		switch {
		case !st.Exists:
			step.Action, step.Reason = ActionCreate, "volume does not exist"
		case st.Empty && len(inUse) == 0:
			step.Action, step.Reason = ActionUnpack, "volume exists and is empty"
		default:
			var why []string
			if !st.Empty {
				why = append(why, "contains data")
			}
			if len(inUse) > 0 {
				why = append(why, "in use by "+strings.Join(users, ", "))
			}
			step.Reason = strings.Join(why, "; ")
			if force {
				step.Action, step.Stop = ActionOverwrite, inUse
			} else {
				step.Action = ActionBlocked
				step.Reason += "; use --force to overwrite"
			}
		}
		if st.Exists {
			driver := v.Driver
			if driver == "" {
				driver = "local"
			}
			if st.Existing.Driver != driver {
				step.Warnings = append(step.Warnings, fmt.Sprintf("driver is %q, archive has %q (cannot be changed)", st.Existing.Driver, driver))
			}
			if !maps.Equal(st.Existing.Options, v.DriverOpts) && (len(st.Existing.Options) > 0 || len(v.DriverOpts) > 0) {
				step.Warnings = append(step.Warnings, "driver options differ from the archive (cannot be changed)")
			}
			if !maps.Equal(st.Existing.Labels, v.Labels) && (len(st.Existing.Labels) > 0 || len(v.Labels) > 0) {
				step.Warnings = append(step.Warnings, "labels differ from the archive (cannot be changed; Compose may warn that it did not create this volume)")
			}
		}
		p.Steps = append(p.Steps, step)
	}
	return p
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/restore/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/restore
git commit -m "Add pure restore planner"
```

---
### Task 13: Restore execution

**Files:**
- Create: `internal/restore/restore.go`, `internal/restore/restore_test.go`

**Interfaces:**
- Consumes: `archive.Open/Verify/OpenVolume`, `selection.Filter`, `stopper.*`, `BuildPlan`, `dockerx.EmptyCheckHelper/ClearHelper/RestoreHelper`.
- Produces:
  - `type Options struct { Archive string; Names, Projects []string; Force, VerifyOnly bool; Image string }`
  - `type Result struct { Restored, Failed, Untouched []string; RestartErrors []error }`
  - `var ErrBlocked = errors.New("restore blocked; nothing changed")`
  - `func Run(ctx context.Context, d dockerx.Docker, out, errOut io.Writer, opts Options) (Result, error)`. With `VerifyOnly`, `d` may be nil.
  - `func PrintPlan(out io.Writer, p Plan)`

- [ ] **Step 1: Write the failing tests**

```go
package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

// writeArchive builds an archive with the given volume contents and Compose project "app".
func writeArchive(t *testing.T, dir string, contents map[string]string) string {
	t.Helper()
	w, err := archive.NewWriter(dir, "src", time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{FormatVersion: manifest.FormatVersion, Tool: "test", CreatedAt: time.Now(), Host: "src", DockerVersion: "29", HelperImage: "img"}
	for _, name := range []string{"app_db", "app_files"} {
		data, ok := contents[name]
		if !ok {
			continue
		}
		entry, n, err := w.AddVolume(name, strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		m.Volumes = append(m.Volumes, manifest.Volume{Name: name, Driver: "local", DriverOpts: map[string]string{},
			Labels: map[string]string{dockerx.LabelComposeProject: "app"}, SizeBytes: n, Archive: entry, Consistent: true})
	}
	if err := w.Finish(m); err != nil {
		t.Fatal(err)
	}
	return w.Path()
}

func run(t *testing.T, f *dockerx.Fake, opts Options) (Result, string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	res, err := Run(context.Background(), f, &out, &errOut, opts)
	return res, out.String(), errOut.String(), err
}

func TestRestoreFreshHost(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	res, out, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Restored, ",") != "app_db,app_files" || len(res.Failed)+len(res.Untouched) != 0 {
		t.Fatalf("res %+v", res)
	}
	if string(f.Data["app_db"]) != "DB" || string(f.Data["app_files"]) != "FILES" {
		t.Fatalf("data %q %q", f.Data["app_db"], f.Data["app_files"])
	}
	if f.Volumes["app_db"].Labels[dockerx.LabelComposeProject] != "app" || f.Volumes["app_db"].Driver != "local" {
		t.Fatalf("volume created without manifest metadata: %+v", f.Volumes["app_db"])
	}
	if strings.Join(f.Calls, " ") != "create-volume app_db helper tar app_db create-volume app_files helper tar app_files" {
		t.Fatalf("calls %v", f.Calls)
	}
	for _, want := range []string{"verifying app_db", "app_db", "create", "volume does not exist", "restored app_db"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestRestoreVerifyOnlyNeedsNoDocker(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB"})
	var out bytes.Buffer
	if _, err := Run(context.Background(), nil, &out, &bytes.Buffer{}, Options{Archive: path, VerifyOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "archive OK") {
		t.Fatalf("out %q", out.String())
	}
}

func TestRestoreBlockedWithoutForce(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local"}, []byte("OLD"))
	f.AddContainer(dockerx.Container{ID: "c", Name: "app-db-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	_, out, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v", err)
	}
	if string(f.Data["app_db"]) != "OLD" || f.Data["app_files"] != nil {
		t.Fatal("blocked plan must change nothing")
	}
	if strings.Contains(strings.Join(f.Calls, " "), "stop") || strings.Contains(strings.Join(f.Calls, " "), "create-volume") {
		t.Fatalf("blocked plan must not touch Docker beyond inspection: %v", f.Calls)
	}
	if !strings.Contains(out, "blocked") || !strings.Contains(out, "app-db-1 (running)") || !strings.Contains(out, "--force") {
		t.Fatalf("plan output:\n%s", out)
	}
}

func TestRestoreForceOverwrites(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local"}, []byte("OLD"))
	f.AddVolume(dockerx.Volume{Name: "app_files", Driver: "local"}, nil)
	f.AddContainer(dockerx.Container{ID: "c", Name: "app-db-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	res, _, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Data["app_db"]) != "DB" || string(f.Data["app_files"]) != "FILES" || len(res.Restored) != 2 {
		t.Fatalf("data %q %q res %+v", f.Data["app_db"], f.Data["app_files"], res)
	}
	calls := strings.Join(f.Calls, " ")
	want := "helper find app_db helper find app_files stop c helper find app_db helper tar app_db helper tar app_files start c"
	if calls != want {
		t.Fatalf("calls:\n got %s\nwant %s", calls, want)
	}
}

func TestRestoreSelectionAndUnknownName(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	if _, _, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Names: []string{"app_files"}}); err != nil {
		t.Fatal(err)
	}
	if f.Data["app_db"] != nil || string(f.Data["app_files"]) != "FILES" {
		t.Fatal("only app_files should be restored")
	}
	if _, _, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Names: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreFailureMidWay(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local"}, nil)
	f.AddContainer(dockerx.Container{ID: "c", Name: "app-db-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	f.FailHelper["tar app_db"] = errors.New("disk on fire")
	res, out, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Force: true})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Join(res.Failed, ",") != "app_db" || strings.Join(res.Untouched, ",") != "app_files" || len(res.Restored) != 0 {
		t.Fatalf("res %+v", res)
	}
	if !strings.Contains(out, "dv-backup restore --force "+path+" app_db app_files") {
		t.Fatalf("retry command missing:\n%s", out)
	}
	cs, _ := f.ListContainers(context.Background())
	if cs[0].State != dockerx.StateRunning {
		t.Fatal("container not restarted after failure")
	}
}

func TestRestoreCorruptArchiveChangesNothing(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB"})
	// Corrupt the first volume entry (data starts at byte 512).
	b, _ := os.ReadFile(path)
	b[600] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f := dockerx.NewFake()
	_, _, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("Docker must not be touched: %v", f.Calls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/restore/`
Expected: FAIL, undefined `Run`, `Options`.

- [ ] **Step 3: Write the implementation**

```go
package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
	"github.com/alexander-jacob/dv-backup/internal/selection"
	"github.com/alexander-jacob/dv-backup/internal/stopper"
)

// ErrBlocked is returned when the plan contains a blocked volume; nothing was changed.
var ErrBlocked = errors.New("restore blocked; nothing changed (see plan above)")

// Options controls a restore run.
type Options struct {
	Archive    string
	Names      []string
	Projects   []string
	Force      bool
	VerifyOnly bool
	Image      string
}

// Result reports what happened per volume.
type Result struct {
	Restored      []string
	Failed        []string
	Untouched     []string
	RestartErrors []error
}

// Run verifies, plans and executes a restore. With VerifyOnly, d may be nil.
func Run(ctx context.Context, d dockerx.Docker, out, errOut io.Writer, opts Options) (res Result, err error) {
	r, err := archive.Open(opts.Archive)
	if err != nil {
		return res, err
	}
	defer r.Close()
	selected, err := selection.Filter(r.Manifest().Volumes,
		func(v manifest.Volume) string { return v.Name },
		func(v manifest.Volume) string { return v.Project() },
		opts.Names, opts.Projects)
	if err != nil {
		return res, err
	}
	for _, v := range selected {
		fmt.Fprintf(out, "verifying %s\n", v.Name)
		if err := r.Verify(v.Name); err != nil {
			return res, err
		}
	}
	if opts.VerifyOnly {
		fmt.Fprintf(out, "archive OK: %d volume(s) verified\n", len(selected))
		return res, nil
	}

	if _, err := d.EnsureImage(ctx, opts.Image); err != nil {
		return res, err
	}
	containers, err := d.ListContainers(ctx)
	if err != nil {
		return res, err
	}
	states := map[string]State{}
	for _, v := range selected {
		st, err := observe(ctx, d, opts.Image, v.Name, containers)
		if err != nil {
			return res, err
		}
		states[v.Name] = st
	}
	plan := BuildPlan(selected, states, opts.Force)
	PrintPlan(out, plan)
	for _, s := range plan.Steps {
		for _, w := range s.Warnings {
			fmt.Fprintf(errOut, "warning: volume %s: %s\n", s.Volume.Name, w)
		}
	}
	if plan.Blocked() {
		return res, ErrBlocked
	}

	st := stopper.New(d, out)
	defer func() {
		res.RestartErrors = st.Restart(context.WithoutCancel(ctx))
	}()
	if err = st.Stop(ctx, plan.ToStop()); err != nil {
		res.Untouched = names(plan.Steps)
		return res, err
	}

	for i, s := range plan.Steps {
		fmt.Fprintf(out, "restoring volume %s (%s)\n", s.Volume.Name, s.Action)
		if err = apply(ctx, d, r, opts.Image, s); err != nil {
			res.Failed = []string{s.Volume.Name}
			res.Untouched = names(plan.Steps[i+1:])
			fmt.Fprintf(out, "\nrestored:  %s\nfailed:    %s\nuntouched: %s\n", orNone(res.Restored), orNone(res.Failed), orNone(res.Untouched))
			fmt.Fprintf(out, "retry with:\n  dv-backup restore --force %s %s\n", opts.Archive, strings.Join(append(res.Failed, res.Untouched...), " "))
			return res, err
		}
		res.Restored = append(res.Restored, s.Volume.Name)
		fmt.Fprintf(out, "restored %s\n", s.Volume.Name)
	}
	return res, nil
}

// observe gathers the host state of one volume.
func observe(ctx context.Context, d dockerx.Docker, image, name string, containers []dockerx.Container) (State, error) {
	existing, exists, err := d.InspectVolume(ctx, name)
	if err != nil {
		return State{}, err
	}
	st := State{Exists: exists, Existing: existing}
	for _, c := range containers {
		if !c.IsHelper() && c.UsesVolume(name) {
			st.Users = append(st.Users, c)
		}
	}
	if exists {
		var buf bytes.Buffer
		hr, err := d.RunHelper(ctx, dockerx.EmptyCheckHelper(image, name, &buf))
		if err != nil {
			return State{}, fmt.Errorf("check volume %s: %w", name, err)
		}
		if hr.ExitCode != 0 {
			return State{}, fmt.Errorf("check volume %s: find exit %d: %s", name, hr.ExitCode, hr.Stderr)
		}
		st.Empty = strings.TrimSpace(buf.String()) == ""
	}
	return st, nil
}

// apply executes one step. Containers are already stopped.
func apply(ctx context.Context, d dockerx.Docker, r *archive.Reader, image string, s Step) error {
	v := s.Volume
	switch s.Action {
	case ActionCreate:
		driver := v.Driver
		if driver == "" {
			driver = "local"
		}
		if err := d.CreateVolume(ctx, dockerx.Volume{Name: v.Name, Driver: driver, Options: v.DriverOpts, Labels: v.Labels}); err != nil {
			return err
		}
	case ActionOverwrite:
		hr, err := d.RunHelper(ctx, dockerx.ClearHelper(image, v.Name))
		if err != nil {
			return fmt.Errorf("clear volume %s: %w", v.Name, err)
		}
		if hr.ExitCode != 0 {
			return fmt.Errorf("clear volume %s: find exit %d: %s", v.Name, hr.ExitCode, hr.Stderr)
		}
	case ActionUnpack:
	case ActionBlocked:
		return ErrBlocked
	}
	rc, err := r.OpenVolume(v.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	hr, err := d.RunHelper(ctx, dockerx.RestoreHelper(image, v.Name, rc))
	if err != nil {
		return fmt.Errorf("unpack volume %s: %w", v.Name, err)
	}
	if hr.ExitCode != 0 {
		return fmt.Errorf("unpack volume %s: tar exit %d: %s", v.Name, hr.ExitCode, hr.Stderr)
	}
	return nil
}

// PrintPlan renders the plan as a table.
func PrintPlan(out io.Writer, p Plan) {
	fmt.Fprintln(out, "\nplan:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VOLUME\tACTION\tREASON")
	for _, s := range p.Steps {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Volume.Name, s.Action, s.Reason)
	}
	tw.Flush()
	fmt.Fprintln(out)
}

func names(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Volume.Name)
	}
	return out
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./... && go test ./internal/restore/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/restore
git commit -m "Add restore execution with plan printing and retry hints"
```

---

### Task 14: stat (host and archive)

**Files:**
- Create: `internal/stat/stat.go`, `internal/stat/stat_test.go`

**Interfaces:**
- Produces: `func Host(ctx context.Context, d dockerx.Docker, out io.Writer) error`; `func Archive(path string, out io.Writer) error`.

- [ ] **Step 1: Write the failing tests**

```go
package stat

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

func TestHost(t *testing.T) {
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local", Labels: map[string]string{dockerx.LabelComposeProject: "app"}}, nil)
	f.AddVolume(dockerx.Volume{Name: "plain", Driver: "local"}, nil)
	f.AddVolume(dockerx.Volume{Name: strings.Repeat("a", 64), Driver: "local", Labels: map[string]string{dockerx.LabelAnonymous: ""}}, nil)
	f.Sizes["app_db"] = 3 << 20
	f.AddContainer(dockerx.Container{ID: "c1", Name: "app-db-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}, {Type: "bind", Source: "/srv/data", Destination: "/data", RW: true}, {Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true}}})
	f.AddContainer(dockerx.Container{ID: "c2", Name: "app-web-1", State: dockerx.StateExited, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	f.AddContainer(dockerx.Container{ID: "h", Name: "dv-backup-helper-dead", State: dockerx.StateExited, Labels: map[string]string{dockerx.LabelHelper: "true"}, Mounts: []dockerx.Mount{{Type: "volume", Name: "plain"}}})
	var out bytes.Buffer
	if err := Host(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"fakehost", "29.8.1", "app_db", "app", "3.0 MiB", "app-db-1 (running)", "app-web-1 (exited)", "plain", "?",
		"anonymous", strings.Repeat("a", 64), "bind", "/srv/data", "stray helper", "dv-backup-helper-dead", "docker rm -f"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "docker.sock") {
		t.Errorf("/var/run bind mounts must be skipped:\n%s", s)
	}
	if strings.Contains(s, "dv-backup-helper-dead (exited)") {
		t.Errorf("helper must not be listed as a volume user:\n%s", s)
	}
}

func TestArchive(t *testing.T) {
	dir := t.TempDir()
	w, err := archive.NewWriter(dir, "vm-app-01", time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	entry, n, _ := w.AddVolume("app_db", strings.NewReader("DATA"))
	m := &manifest.Manifest{FormatVersion: 1, Tool: "dv-backup v0.1.0", CreatedAt: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC), Host: "vm-app-01", DockerVersion: "29.8.1", HelperImage: "img@sha256:x",
		Volumes: []manifest.Volume{{Name: "app_db", Driver: "local", Labels: map[string]string{dockerx.LabelComposeProject: "app"}, SizeBytes: n, Archive: entry, Consistent: false}}}
	if err := w.Finish(m); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Archive(w.Path(), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"vm-app-01", "2026-09-17T12:00:00Z", "29.8.1", "dv-backup v0.1.0", "img@sha256:x", "app_db", "app", entry.SHA256, "warning", "not consistent"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/stat/`
Expected: FAIL, undefined `Host`.

- [ ] **Step 3: Write the implementation**

```go
// Package stat renders what exists on a host and what an archive contains.
package stat

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
	"github.com/alexander-jacob/dv-backup/internal/selection"
)

var skippedBindPrefixes = []string{"/var/run", "/run", "/dev", "/proc", "/sys"}

// Host prints the named volumes of the daemon with sizes and users, then warnings.
func Host(ctx context.Context, d dockerx.Docker, out io.Writer) error {
	info, err := d.Info(ctx)
	if err != nil {
		return err
	}
	vols, err := d.ListVolumes(ctx)
	if err != nil {
		return err
	}
	sizes, err := d.VolumeSizes(ctx)
	if err != nil {
		return err
	}
	containers, err := d.ListContainers(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "host %s, Docker %s\n\n", info.Name, info.ServerVersion)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VOLUME\tPROJECT\tDRIVER\tSIZE\tCONTAINERS")
	for _, v := range selection.Named(vols) {
		size := int64(-1)
		if s, ok := sizes[v.Name]; ok {
			size = s
		}
		var users []string
		for _, c := range containers {
			if !c.IsHelper() && c.UsesVolume(v.Name) {
				users = append(users, fmt.Sprintf("%s (%s)", c.Name, c.State))
			}
		}
		sort.Strings(users)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, v.Labels[dockerx.LabelComposeProject], v.Driver, manifest.FormatBytes(size), strings.Join(users, ", "))
	}
	tw.Flush()

	var warnings []string
	for _, v := range vols {
		if selection.IsAnonymous(v) {
			warnings = append(warnings, fmt.Sprintf("anonymous volume %s is not backed up", v.Name))
		}
	}
	var stray []string
	for _, c := range containers {
		if c.IsHelper() {
			stray = append(stray, c.Name)
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "bind" && m.RW && !skipBind(m.Source) {
				warnings = append(warnings, fmt.Sprintf("container %s has writable bind mount %s -> %s; bind mounts are not backed up", c.Name, m.Source, m.Destination))
			}
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		warnings = append(warnings, fmt.Sprintf("stray helper container(s) %s from an interrupted run; remove with: docker rm -f $(docker ps -aq --filter label=%s=true)", strings.Join(stray, ", "), dockerx.LabelHelper))
	}
	if len(warnings) > 0 {
		fmt.Fprintln(out, "\nwarnings:")
		for _, w := range warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	return nil
}

func skipBind(source string) bool {
	for _, p := range skippedBindPrefixes {
		if source == p || strings.HasPrefix(source, p+"/") {
			return true
		}
	}
	return false
}

// Archive prints the manifest of an archive without reading volume data.
func Archive(path string, out io.Writer) error {
	r, err := archive.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	m := r.Manifest()
	fmt.Fprintf(out, "archive %s\n  host: %s\n  created: %s\n  docker: %s\n  tool: %s\n  helper image: %s\n\n",
		path, m.Host, m.CreatedAt.UTC().Format(time.RFC3339), m.DockerVersion, m.Tool, m.HelperImage)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VOLUME\tPROJECT\tDATA\tCOMPRESSED\tCONSISTENT\tSHA256")
	var warnings []string
	for _, v := range m.Volumes {
		cons := "yes"
		if !v.Consistent {
			cons = "no"
			warnings = append(warnings, fmt.Sprintf("volume %s is not consistent (backed up while in use)", v.Name))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Project(), manifest.FormatBytes(v.SizeBytes), manifest.FormatBytes(v.Archive.SizeBytes), cons, v.Archive.SHA256)
	}
	tw.Flush()
	if len(warnings) > 0 {
		fmt.Fprintln(out, "\nwarnings:")
		for _, w := range warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./... && go test ./internal/stat/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stat
git commit -m "Add host and archive stat rendering"
```

---
### Task 15: CLI wiring

**Files:**
- Modify: `cmd/dv-backup/main.go`, `cmd/dv-backup/main_test.go`

**Interfaces:**
- Consumes: `backup.Run`, `restore.Run`, `restore.ErrBlocked`, `stat.Host`, `stat.Archive`, `dockerx.Connect`, `dockerx.DefaultImage`.
- Produces: commands `stat`, `backup`, `restore` with the flags from spec §3; global `--image` (env `DV_BACKUP_IMAGE`); exit codes per §7.4. `newRoot` gains a `connect func() (dockerx.Docker, error)` field on a small `app` struct so tests can inject the fake.

- [ ] **Step 1: Extend the failing tests**

Append to `cmd/dv-backup/main_test.go`:

```go
func runWith(t *testing.T, d dockerx.Docker, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	a := &app{stdout: &out, stderr: &errOut, connect: func() (dockerx.Docker, error) { return d, nil }}
	code := a.run(context.Background(), args)
	return code, out.String(), errOut.String()
}

func TestHelpMentionsBothRestoreFlows(t *testing.T) {
	_, out, _ := runWith(t, nil, "--help")
	for _, want := range []string{"before deploy", "after deploy", "--force", "DV_BACKUP_IMAGE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help lacks %q:\n%s", want, out)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	f := dockerx.NewFake()
	for _, args := range [][]string{{"restore"}, {"backup", "--bogus"}, {"stat", "extra"}, {"restore", "a", "--verify-only", "--force", "x"}} {
		if code, _, _ := runWith(t, f, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestStatRunsAgainstFake(t *testing.T) {
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "v", Driver: "local"}, nil)
	code, out, _ := runWith(t, f, "stat")
	if code != 0 || !strings.Contains(out, "v") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestBackupExitCodes(t *testing.T) {
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "v", Driver: "local"}, []byte("X"))
	f.AddContainer(dockerx.Container{ID: "c", Name: "c", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "v"}}})
	dir := t.TempDir()
	if code, out, _ := runWith(t, f, "backup", "-o", dir, "v"); code != 0 || !strings.Contains(out, "wrote ") {
		t.Fatalf("code %d out %q", code, out)
	}
	if code, _, errOut := runWith(t, f, "backup", "-o", dir, "nope"); code != 1 || !strings.Contains(errOut, "nope") {
		t.Fatalf("unknown volume: code %d err %q", code, errOut)
	}
	f.FailStart["c"] = errors.New("no start")
	if code, _, errOut := runWith(t, f, "backup", "-o", t.TempDir(), "v"); code != 3 || !strings.Contains(errOut, "no start") {
		t.Fatalf("restart failure: code %d err %q", code, errOut)
	}
}

func TestRestoreExitCodes(t *testing.T) {
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "v", Driver: "local"}, []byte("X"))
	dir := t.TempDir()
	code, out, _ := runWith(t, f, "backup", "-o", dir, "v")
	if code != 0 {
		t.Fatal(out)
	}
	path := strings.TrimSpace(strings.TrimPrefix(out[strings.LastIndex(out, "wrote "):], "wrote "))
	if code, _, _ := runWith(t, f, "restore", "--verify-only", path); code != 0 {
		t.Fatalf("verify-only: %d", code)
	}
	if code, _, _ := runWith(t, f, "restore", path); code != 1 {
		t.Fatalf("blocked restore must exit 1, got %d", code)
	}
	if code, _, _ := runWith(t, f, "restore", "--force", path); code != 0 {
		t.Fatalf("forced restore: %d", code)
	}
	if code, _, errOut := runWith(t, f, "restore", "/nonexistent.tar"); code != 1 || errOut == "" {
		t.Fatalf("missing archive: %d %q", code, errOut)
	}
}
```

The import block of `main_test.go` becomes `"bytes"`, `"context"`, `"errors"`, `"strings"`, `"testing"` and `"github.com/alexander-jacob/dv-backup/internal/dockerx"`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/dv-backup/`
Expected: FAIL, undefined `app`.

- [ ] **Step 3: Rewrite main.go**

```go
// Command dv-backup backs up and restores named Docker volumes.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alexander-jacob/dv-backup/internal/backup"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/restore"
	"github.com/alexander-jacob/dv-backup/internal/stat"
	"github.com/alexander-jacob/dv-backup/internal/version"
)

const longHelp = `dv-backup backs up named Docker volumes into one archive file and restores
them. Volume data is read and written through a short-lived helper container
running GNU tar (image: --image / DV_BACKUP_IMAGE, default ` + dockerx.DefaultImage + `),
so ownership, modes, links, sparse files and xattrs are preserved.

By default backup stops every container that uses a selected volume, archives
the volumes and restarts exactly those containers afterwards, in the order they
were originally started. Archives are not encrypted.

Restore flows for a host rebuilt by Terraform and deployed by a pipeline:

  before deploy:   taint -> apply -> scp archive -> dv-backup restore -> run pipeline
                   Volumes are created with the driver and labels from the archive,
                   so Compose adopts them.

  after deploy:    taint -> apply -> run pipeline (or compose up --no-start)
                   -> scp archive -> dv-backup restore --force -> start stack
                   --force is normally required here: Docker copies image content
                   into new volumes when the container is *created*, and database
                   images initialise their data directory on first start, so the
                   volumes are no longer empty.

Without --force, restore refuses to touch a volume that contains data or is in
use (running, paused or restarting container) and changes nothing at all.

Exit codes: 0 ok, 1 error, 2 usage, 3 finished but a stopped container could
not be restarted (start it by hand: see the output).
`

type app struct {
	stdout, stderr io.Writer
	connect        func() (dockerx.Docker, error)
	image          string
}

// exitError carries an exit code from a command to run().
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func fail(code int, err error) error { return &exitError{code: code, err: err} }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{stdout: stdout, stderr: stderr, connect: func() (dockerx.Docker, error) { return dockerx.Connect() }}
	return a.run(ctx, args)
}

func (a *app) run(ctx context.Context, args []string) int {
	root := a.newRoot(ctx)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(a.stderr, "error:", ee.err)
		return ee.code
	}
	fmt.Fprintln(a.stderr, "error:", err)
	return 2
}

func (a *app) docker() (dockerx.Docker, error) {
	d, err := a.connect()
	if err != nil {
		return nil, fail(1, err)
	}
	return d, nil
}

func (a *app) newRoot(ctx context.Context) *cobra.Command {
	root := &cobra.Command{
		Use:           "dv-backup",
		Short:         "Back up and restore named Docker volumes",
		Long:          longHelp,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetContext(ctx)
	root.SetVersionTemplate("dv-backup {{.Version}}\n")
	defaultImage := os.Getenv("DV_BACKUP_IMAGE")
	if defaultImage == "" {
		defaultImage = dockerx.DefaultImage
	}
	root.PersistentFlags().StringVar(&a.image, "image", defaultImage, "helper image with GNU tar (env DV_BACKUP_IMAGE)")
	root.AddCommand(a.statCmd(), a.backupCmd(), a.restoreCmd())
	return root
}

func (a *app) statCmd() *cobra.Command {
	var archivePath string
	cmd := &cobra.Command{
		Use:   "stat",
		Short: "Show named volumes on this host, or the contents of an archive",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if archivePath != "" {
				if err := stat.Archive(archivePath, a.stdout); err != nil {
					return fail(1, err)
				}
				return nil
			}
			d, err := a.docker()
			if err != nil {
				return err
			}
			defer d.Close()
			if err := stat.Host(cmd.Context(), d, a.stdout); err != nil {
				return fail(1, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&archivePath, "archive", "", "show the manifest of this archive instead of the host")
	return cmd
}

func (a *app) backupCmd() *cobra.Command {
	opts := backup.Options{}
	cmd := &cobra.Command{
		Use:   "backup [volume...]",
		Short: "Back up all named volumes, or only the listed ones",
		Example: "  dv-backup backup -o /var/backups\n  dv-backup backup -p n8n -p grafana\n  dv-backup backup --no-stop n8n_n8n_data",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.docker()
			if err != nil {
				return err
			}
			defer d.Close()
			opts.Names, opts.Image = args, a.image
			res, err := backup.Run(cmd.Context(), d, a.stdout, a.stderr, opts)
			return a.finish(res.RestartErrors, err)
		},
	}
	cmd.Flags().StringVarP(&opts.OutputDir, "output", "o", ".", "output directory")
	cmd.Flags().StringArrayVarP(&opts.Projects, "project", "p", nil, "only volumes of this Compose project (repeatable)")
	cmd.Flags().BoolVar(&opts.NoStop, "no-stop", false, "do not stop containers; affected volumes are marked inconsistent")
	return cmd
}

func (a *app) restoreCmd() *cobra.Command {
	opts := restore.Options{}
	cmd := &cobra.Command{
		Use:   "restore <file> [volume...]",
		Short: "Restore all volumes from an archive, or only the listed ones",
		Example: "  dv-backup restore dv-backup-vm-app-01-20260917T120000Z.tar\n  dv-backup restore --force backup.tar\n  dv-backup restore --verify-only backup.tar\n  dv-backup restore -p n8n backup.tar",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.VerifyOnly && opts.Force {
				return errors.New("--verify-only and --force cannot be combined")
			}
			opts.Archive, opts.Names, opts.Image = args[0], args[1:], a.image
			var d dockerx.Docker
			if !opts.VerifyOnly {
				var err error
				if d, err = a.docker(); err != nil {
					return err
				}
				defer d.Close()
			}
			res, err := restore.Run(cmd.Context(), d, a.stdout, a.stderr, opts)
			return a.finish(res.RestartErrors, err)
		},
	}
	cmd.Flags().StringArrayVarP(&opts.Projects, "project", "p", nil, "only volumes of this Compose project (repeatable)")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "overwrite volumes that contain data or are in use; stops and restarts their containers")
	cmd.Flags().BoolVar(&opts.VerifyOnly, "verify-only", false, "verify manifest and checksums, change nothing (needs no Docker)")
	return cmd
}

// finish maps an operation outcome to an exit code (spec §7.4).
func (a *app) finish(restartErrs []error, err error) error {
	for _, e := range restartErrs {
		fmt.Fprintln(a.stderr, "error:", e)
	}
	if err != nil {
		return fail(1, err)
	}
	if len(restartErrs) > 0 {
		return fail(3, fmt.Errorf("%d container(s) could not be restarted; start them by hand", len(restartErrs)))
	}
	return nil
}
```

`TestNoArgsPrintsUsage` from Task 1 still passes: `RunE` prints help. `restore.ErrBlocked` reaches `finish` as a normal error and yields exit 1, which is what the spec wants.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS for every package.

- [ ] **Step 5: Manual smoke test on the workstation (read-only)**

```bash
go build -o /tmp/dv-backup ./cmd/dv-backup && /tmp/dv-backup stat | head -20 && /tmp/dv-backup --help | head -5
```

Expected: a table of volumes, a warnings section, no error. Do **not** run `backup` or `restore` here without explicit `dv-backup-test-*` volume names.

- [ ] **Step 6: Commit**

```bash
git add cmd/dv-backup
git commit -m "Wire stat, backup and restore commands with exit codes"
```

---

### Task 16: Integration tests against real Docker

**Files:**
- Create: `internal/integration/harness_test.go`, `internal/integration/fidelity_test.go`, `internal/integration/containers_test.go`, `internal/integration/restore_test.go`, `internal/integration/signal_test.go`

All files start with `//go:build integration` and `package integration`. Every resource is named `dv-backup-test-<hex>` and removed in `t.Cleanup`. Test images: `debian:13-slim` (already used as helper) and `nginx:alpine` (copy-up test); both are pulled by the tests through `EnsureImage` when missing.

- [ ] **Step 1: Write the harness**

`internal/integration/harness_test.go`:

```go
//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/alexander-jacob/dv-backup/internal/backup"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/restore"
)

const prefix = "dv-backup-test-"

// Spec Appendix A, with big.bin reduced to 5 MB.
const populateScript = `set -e
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
head -c 5M /dev/urandom > big.bin
touch -h -d '2020-01-02 03:04:05' pgdata/PG_VERSION exec.sh hard1 rel-link big.bin
chown 999:999 /data && chmod 0700 /data
`

const inspectScript = `cd /data && find . -printf '%U:%G %#m %y %n %Ts %l %p\n' | sort -k7
echo "--- sums"; find . -type f -exec md5sum {} + | sort -k2
echo "--- disk usage (sparse check)"; du -sk sparse.img big.bin 2>/dev/null
`

type harness struct {
	t   *testing.T
	ctx context.Context
	d   *dockerx.Client
	raw *client.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	d, err := dockerx.Connect()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, d: d, raw: raw}
	if _, err := d.EnsureImage(ctx, dockerx.DefaultImage); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) name() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// volume creates a volume with labels and registers its removal.
func (h *harness) volume(labels map[string]string) string {
	name := h.name()
	if err := h.d.CreateVolume(h.ctx, dockerx.Volume{Name: name, Driver: "local", Labels: labels}); err != nil {
		h.t.Fatal(err)
	}
	h.cleanupVolume(name)
	return name
}

func (h *harness) cleanupVolume(name string) {
	if !strings.HasPrefix(name, prefix) {
		h.t.Fatalf("refusing to manage volume %q outside test prefix", name)
	}
	h.t.Cleanup(func() {
		_, _ = h.raw.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true})
	})
}

func (h *harness) sh(vol, script string) string {
	var out bytes.Buffer
	res, err := h.d.RunHelper(h.ctx, dockerx.Helper{Image: dockerx.DefaultImage, Volume: vol, Entrypoint: "sh", Args: []string{"-c", script}, Stdout: &out})
	if err != nil {
		h.t.Fatal(err)
	}
	if res.ExitCode != 0 {
		h.t.Fatalf("script exit %d: %s", res.ExitCode, res.Stderr)
	}
	return out.String()
}

func (h *harness) populate(vol string) { h.sh(vol, populateScript) }
func (h *harness) listing(vol string) string { return h.sh(vol, inspectScript) }

// container creates (and optionally starts) a sleeping container that mounts vol.
func (h *harness) container(vol string, start bool, labels map[string]string) string {
	name := h.name()
	if !strings.HasPrefix(name, prefix) {
		h.t.Fatal("bad name")
	}
	res, err := h.raw.ContainerCreate(h.ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{Image: dockerx.DefaultImage, Cmd: []string{"sleep", "600"}, Labels: labels},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: "/mnt/vol"}}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		_, _ = h.raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
	})
	if start {
		if _, err := h.raw.ContainerStart(h.ctx, res.ID, client.ContainerStartOptions{}); err != nil {
			h.t.Fatal(err)
		}
	}
	return res.ID
}

func (h *harness) state(id string) string {
	res, err := h.raw.ContainerInspect(h.ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return string(res.Container.State.Status)
}

func (h *harness) startedAt(id string) time.Time {
	t, err := h.d.StartedAt(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

func (h *harness) backup(dir string, vols ...string) (backup.Result, string, error) {
	if len(vols) == 0 {
		h.t.Fatal("integration tests must always pass explicit volume names to backup")
	}
	var out, errOut bytes.Buffer
	res, err := backup.Run(h.ctx, h.d, &out, &errOut, backup.Options{OutputDir: dir, Names: vols, Image: dockerx.DefaultImage})
	return res, out.String() + errOut.String(), err
}

func (h *harness) restore(path string, force bool, vols ...string) (restore.Result, string, error) {
	if len(vols) == 0 {
		h.t.Fatal("integration tests must always pass explicit volume names to restore")
	}
	var out, errOut bytes.Buffer
	res, err := restore.Run(h.ctx, h.d, &out, &errOut, restore.Options{Archive: path, Names: vols, Force: force, Image: dockerx.DefaultImage})
	return res, out.String() + errOut.String(), err
}
```

- [ ] **Step 2: Write the fidelity tests**

`internal/integration/fidelity_test.go`:

```go
//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

func TestRoundTripFidelity(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(map[string]string{"com.docker.compose.project": "dvtest", "com.docker.compose.volume": "data"})
	h.populate(vol)
	before := h.listing(vol)
	if !strings.Contains(before, "sparse.img") || !strings.Contains(before, "999:999") {
		t.Fatalf("fixture incomplete:\n%s", before)
	}

	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := h.raw.VolumeRemove(h.ctx, vol, client.VolumeRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, out, err := h.restore(res.Path, false, vol); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	after := h.listing(vol)
	if before != after {
		t.Fatalf("listing differs\n--- before\n%s\n--- after\n%s", before, after)
	}
	v, ok, err := h.d.InspectVolume(h.ctx, vol)
	if err != nil || !ok || v.Labels["com.docker.compose.project"] != "dvtest" || v.Labels["com.docker.compose.volume"] != "data" {
		t.Fatalf("labels not restored: %+v ok=%v err=%v", v, ok, err)
	}
	// Sparse file still sparse: du reports far less than 100 MB.
	for _, line := range strings.Split(after, "\n") {
		if strings.HasSuffix(line, "sparse.img") {
			kb := strings.Fields(line)[0]
			if len(kb) > 4 { // more than 9999 KiB means it was expanded
				t.Fatalf("sparse file expanded: %s", line)
			}
		}
	}
}

// Xattrs travel as PAX records: build a tar with them in Go, push it through the
// restore helper, back the volume up and check the records survived.
func TestXattrsRoundTrip(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	capNetRaw := "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" // VFS_CAP_REVISION_2, cap_net_raw=ep
	_ = tw.WriteHeader(&tar.Header{Name: "./x", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		PAXRecords: map[string]string{"SCHILY.xattr.user.dvtest": "hello", "SCHILY.xattr.security.capability": capNetRaw}})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if r, err := h.d.RunHelper(h.ctx, dockerx.RestoreHelper(dockerx.DefaultImage, vol, &buf)); err != nil || r.ExitCode != 0 {
		t.Fatalf("restore helper: %+v %v", r, err)
	}
	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ar, err := archive.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	rc, err := ar.OpenVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatal("./x not found in backup stream")
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != "./x" {
			continue
		}
		if hdr.PAXRecords["SCHILY.xattr.user.dvtest"] != "hello" {
			t.Fatalf("user xattr lost: %v", hdr.PAXRecords)
		}
		if hdr.PAXRecords["SCHILY.xattr.security.capability"] != capNetRaw {
			t.Fatalf("security.capability lost (see spec 10.2; consider --xattrs-include='*'): %q", hdr.PAXRecords["SCHILY.xattr.security.capability"])
		}
		return
	}
}
```

- [ ] **Step 3: Write the container tests**

`internal/integration/containers_test.go`:

```go
//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/backup"
)

func TestRunningContainerStoppedAndRestarted(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	h.sh(vol, "echo data > /data/f")
	id := h.container(vol, true, map[string]string{"com.docker.compose.service": "web"})
	firstStart := h.startedAt(id)

	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.state(id) != "running" {
		t.Fatalf("container state after backup: %s", h.state(id))
	}
	if !h.startedAt(id).After(firstStart) {
		t.Fatal("container was not restarted (StartedAt unchanged)")
	}
	ar, _ := archive.Open(res.Path)
	defer ar.Close()
	v, _ := ar.Manifest().Find(vol)
	if len(v.Containers) != 1 || !v.Containers[0].WasRunning || v.Containers[0].ComposeService != "web" {
		t.Fatalf("manifest containers: %+v", v.Containers)
	}
	if !strings.Contains(out, "stopping container") || !strings.Contains(out, "starting container") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestFailedBackupStillRestarts(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	id := h.container(vol, true, nil)
	var out, errOut strings.Builder
	// hello-world has no tar binary: the helper fails to start.
	_, err := backup.Run(h.ctx, h.d, &out, &errOut, backup.Options{OutputDir: t.TempDir(), Names: []string{vol}, Image: "hello-world"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if h.state(id) != "running" {
		t.Fatalf("container not restarted after failed backup: %s", h.state(id))
	}
}

func TestPausedContainerIsStoppedAndComesBackRunning(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	id := h.container(vol, true, nil)
	if _, err := h.raw.ContainerPause(h.ctx, id, client.ContainerPauseOptions{}); err != nil {
		t.Fatal(err)
	}
	if h.state(id) != string(container.StatePaused) {
		t.Fatal("precondition: container not paused")
	}
	_, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "(paused)") {
		t.Fatalf("output should show the paused state:\n%s", out)
	}
	if h.state(id) != "running" {
		t.Fatalf("state after backup: %s", h.state(id))
	}
}
```

`client.ContainerPauseOptions{}` is the verified options type for `ContainerPause`.

- [ ] **Step 4: Write the restore tests**

`internal/integration/restore_test.go`:

```go
//go:build integration

package integration

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/restore"
)

func TestCorruptArchiveRefusedBeforeAnyChange(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	h.sh(vol, "echo data > /data/f")
	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := h.raw.VolumeRemove(h.ctx, vol, client.VolumeRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	b[512+100] ^= 0xff
	if err := os.WriteFile(res.Path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.restore(res.Path, false, vol); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := h.d.InspectVolume(h.ctx, vol); ok {
		t.Fatal("volume must not have been created")
	}
}

func TestBlockedThenForced(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	h.sh(vol, "echo new > /data/f")
	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	h.sh(vol, "rm /data/f && echo old > /data/g")
	id := h.container(vol, true, nil)

	_, out, err = h.restore(res.Path, false, vol)
	if !errors.Is(err, restore.ErrBlocked) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if h.sh(vol, "ls /data") != "g\n" {
		t.Fatal("blocked restore changed the volume")
	}

	_, out, err = h.restore(res.Path, true, vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.sh(vol, "ls /data") != "f\n" || h.sh(vol, "cat /data/f") != "new\n" {
		t.Fatalf("volume not overwritten: %s", h.sh(vol, "ls -la /data"))
	}
	if h.state(id) != "running" {
		t.Fatalf("container state: %s", h.state(id))
	}
}

// Spec §10.1: Docker copies image content into a new named volume when the
// container is created, so restore after `compose up --no-start` needs --force.
func TestRestoreAfterCreateNoStartNeedsForce(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	h.sh(vol, "echo mine > /data/index.html")
	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := h.raw.VolumeRemove(h.ctx, vol, client.VolumeRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.EnsureImage(h.ctx, "nginx:alpine"); err != nil {
		t.Fatal(err)
	}
	// Recreate the volume under its original name (its cleanup is already
	// registered) and create, but do not start, a container whose image has
	// files at the mount path.
	if err := h.d.CreateVolume(h.ctx, dockerx.Volume{Name: vol, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	c, err := h.raw.ContainerCreate(h.ctx, client.ContainerCreateOptions{
		Name:       h.name(),
		Config:     &container.Config{Image: "nginx:alpine"},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: "/usr/share/nginx/html"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.raw.ContainerRemove(h.ctx, c.ID, client.ContainerRemoveOptions{Force: true}) })

	if _, _, err := h.restore(res.Path, false, vol); !errors.Is(err, restore.ErrBlocked) {
		t.Fatalf("expected blocked (copy-up made the volume non-empty), got %v", err)
	}
	if _, out, err := h.restore(res.Path, true, vol); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.sh(vol, "cat /data/index.html") != "mine\n" || strings.Contains(h.sh(vol, "ls /data"), "50x.html") {
		t.Fatalf("volume content after forced restore: %s", h.sh(vol, "ls /data"))
	}
}
```

- [ ] **Step 5: Write the SIGINT test**

`internal/integration/signal_test.go`:

```go
//go:build integration

package integration

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSIGINTDuringBackupRestartsAndCleans(t *testing.T) {
	h := newHarness(t)
	bin := filepath.Join(t.TempDir(), "dv-backup")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/dv-backup")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	vol := h.volume(nil)
	h.sh(vol, "head -c 600M /dev/urandom > /data/big.bin") // incompressible, takes a few seconds to stream
	id := h.container(vol, true, nil)

	outDir := t.TempDir()
	cmd := exec.Command(bin, "backup", "-o", outDir, vol)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "backing up volume") {
			break
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	for sc.Scan() {
	}
	err = cmd.Wait()
	if err == nil {
		t.Fatal("backup should have been interrupted (if it finished, increase the fixture size)")
	}
	if h.state(id) != "running" {
		t.Fatalf("container not restarted after SIGINT: %s", h.state(id))
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Fatalf("output dir not clean: %v", entries)
	}
}
```

- [ ] **Step 6: Run the integration tests**

Run: `go vet -tags integration ./... && go test -tags integration ./internal/integration/ -v -count=1`
Expected: all PASS within a few minutes. Afterwards confirm no leftovers:

```bash
docker volume ls -q | grep '^dv-backup-test-' ; docker ps -aq --filter name=dv-backup-test- ; docker ps -aq --filter label=dv-backup.helper=true
```

Expected: no output from any of the three.

If `TestXattrsRoundTrip` fails only on `security.capability`, add `"--xattrs-include=*"` to `RestoreHelper` and `BackupHelper` in `dockerx.go`, rerun, and record the outcome in spec §10.2.

- [ ] **Step 7: Commit**

```bash
git add internal/integration
git commit -m "Add integration tests against a live Docker daemon"
```

---
### Task 17: Lint, CI, release and README

**Files:**
- Create: `.golangci.yml`, `.goreleaser.yaml`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `README.md`

**Interfaces:**
- Consumes: `internal/version.Version` (ldflags target).

- [ ] **Step 1: Install and configure the linter**

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

`.golangci.yml`:

```yaml
version: "2"
linters:
  default: standard
  enable:
    - misspell
    - unconvert
    - unparam
```

Run: `golangci-lint run ./...`
Expected: no findings. Fix anything reported (unused helpers such as `bytesReader` or the `errors`/`os` placeholders in `backup.go` are the likely candidates). If the v2 config schema rejects a key, run `golangci-lint config verify` and follow its message.

- [ ] **Step 2: GoReleaser config**

`.goreleaser.yaml`:

```yaml
version: 2
project_name: dv-backup
builds:
  - main: ./cmd/dv-backup
    binary: dv-backup
    env:
      - CGO_ENABLED=0
    goos: [linux]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w -X github.com/alexander-jacob/dv-backup/internal/version.Version={{.Version}}
archives:
  - formats: [binary]
    name_template: "{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}"
checksum:
  name_template: checksums.txt
release:
  draft: false
changelog:
  use: git
```

Verify locally that the ldflags target works:

```bash
go build -ldflags "-X github.com/alexander-jacob/dv-backup/internal/version.Version=v0.0.0-test" -o /tmp/dv-backup ./cmd/dv-backup && /tmp/dv-backup --version
```

Expected: `dv-backup v0.0.0-test`.

- [ ] **Step 3: CI workflow**

`.github/workflows/ci.yml`:

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - uses: golangci/golangci-lint-action@v8
        with:
          version: latest
      - run: go test ./...
      - run: docker info --format '{{.ServerVersion}}'
      - run: go test -tags integration ./internal/integration/ -v -count=1
```

`.github/workflows/release.yml`:

```yaml
name: release
on:
  push:
    tags: ["v*"]
permissions:
  contents: write
jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 4: README**

`README.md` must contain these sections, written out in full:

1. **What it does** (two paragraphs from spec §1 and §2, including the non-goals list).
2. **Install**:
   ```bash
   VERSION=v0.1.0
   curl -fsSLO https://github.com/alexander-jacob/dv-backup/releases/download/${VERSION}/dv-backup-linux-amd64
   curl -fsSLO https://github.com/alexander-jacob/dv-backup/releases/download/${VERSION}/checksums.txt
   sha256sum --check --ignore-missing checksums.txt
   install -m 0755 dv-backup-linux-amd64 /usr/local/bin/dv-backup
   ```
3. **Usage**: the command table from spec §3 verbatim, then both restore flows from spec §1 with the copy-up explanation from §10.1.
4. **How it works**: helper container, archive layout from §6, the manifest example from §6.
5. **Safety and limitations**, one bullet each: archives are not encrypted and are created with mode 0600; containers in state running, paused or restarting are stopped and restarted (paused ones come back running); restart does not wait for health checks; a SIGKILL leaves containers stopped and possibly a helper container, with the two cleanup commands from spec §7.2; image pulls are anonymous, pre-pull for private registries; bind mounts, anonymous volumes and Compose `external: true` volumes (not selectable by `--project`); `--no-stop` consistency; rootless Docker and Windows unsupported; `debian:13-slim` end of life.
6. **Exit codes** table from §7.4.
7. **Development**: `go test ./...`, `go test -tags integration ./internal/integration/`, the `dv-backup-test-` prefix rule, and a warning never to run an unfiltered backup or forced restore on a workstation.

- [ ] **Step 5: Full verification**

```bash
go vet ./... && golangci-lint run ./... && go test ./... && go test -tags integration ./internal/integration/ -count=1
```

Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add .golangci.yml .goreleaser.yaml .github README.md
git commit -m "Add lint config, CI, release workflow and README"
```

Then tell the maintainer: the branch is ready to push once the commit email question from spec Appendix B is settled; the first tag `v0.1.0` triggers the release workflow.

---

## Self-review notes

**Spec coverage.** §3 CLI → Task 15; §5 helper mechanism → Tasks 6 and 8; §6 archive and manifest → Tasks 2 to 5; §7.1 stat → Task 14; §7.2 backup (preflight, select, stop, archive, fail-whole, always restart, free-space warning, exit codes) → Tasks 10, 11, 15; §7.3 restore (verify, plan table, stop-before-change, execute, mid-way failure with retry command, restart) → Tasks 12, 13; §7.4 exit codes → Task 15; §8 unit tests → each task; §8 integration tests → Task 16 (fidelity, labels, running stopped/restarted, failed backup restarts, SIGINT, corrupt archive, blocked/force, copy-up after create, paused, xattrs); §9 CI and release → Task 17; §11.3 classification → Tasks 6 and 9; §11.4 host name, 0600, temp files, sanitising, digest, unknown size → Tasks 4, 7, 11, 14.

**Known deviations from the spec text, all deliberate:**
- Two extra packages, `internal/selection` and `internal/stopper`, hold logic the spec assigns to `backup`/`restore`, because both commands need it.
- The name pattern uses `*` instead of `+` after the first character so one-character volume names are accepted (spec §10.9).
- `dockerx.Fake` lives in a non-test file so other packages' tests can import it; it is not compiled into the binary because nothing in `cmd/` references it, but it does ship in the module. Acceptable for an internal package.

**Verified against the module sources on 2026-09-17:** `container.InspectResponse.State` is a pointer (Task 7 guards nil); `client.ContainerPauseOptions` and `client.VolumeRemoveOptions{Force}` exist (Task 16); `zstd.WithEncoderLevel(zstd.SpeedDefault)` and `(*zstd.Decoder).IOReadCloser()` exist in klauspost/compress v1.20.0 (Tasks 4, 5); `cerrdefs.IsNotFound` is how the moby client's own tests classify 404s (Task 7). `HostConfig.NetworkMode` is `container.NetworkMode`, a string type, so the untyped constant `"none"` converts implicitly (Task 8). The plan's snippets were not compiled; expect small fixes such as unused imports, and use `go doc` for anything else that looks off.
