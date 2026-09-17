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
//
// --xattrs-include='*' is required in addition to --xattrs: GNU tar's default
// xattr filter only includes user.* on extraction (and only excludes a
// blocklist, not an allowlist, on creation), so without it a security.*
// xattr such as security.capability is silently dropped. Verified by
// TestXattrsRoundTrip (spec 10.2).
func BackupHelper(image, vol string, stdout io.Writer) Helper {
	return Helper{Image: image, Volume: vol, ReadOnly: true, Entrypoint: "tar",
		Args: []string{"--numeric-owner", "--xattrs", "--xattrs-include=*", "--acls", "--sparse", "-C", DataDir, "-cpf", "-", "."}, Stdout: stdout}
}

// RestoreHelper extracts a GNU tar archive from stdin into the volume.
func RestoreHelper(image, vol string, stdin io.Reader) Helper {
	return Helper{Image: image, Volume: vol, Entrypoint: "tar",
		Args: []string{"--numeric-owner", "--xattrs", "--xattrs-include=*", "--acls", "-C", DataDir, "-xpf", "-"}, Stdin: stdin}
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
