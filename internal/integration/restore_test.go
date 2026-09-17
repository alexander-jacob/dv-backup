//go:build integration

package integration

import (
	"context"
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
	// Offset 512+4 lands inside the first entry's zstd frame (the compressed
	// tar stream of one small file can be shorter than 100 bytes, which would
	// put a flip at 512+100 in tar padding instead).
	b[512+4] ^= 0xff
	if err := os.WriteFile(res.Path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(res.Path, false, vol); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
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
	id := h.container(vol, nil)

	out, err = h.restore(res.Path, false, vol)
	if !errors.Is(err, restore.ErrBlocked) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if h.sh(vol, "ls /data") != "g\n" {
		t.Fatal("blocked restore changed the volume")
	}

	out, err = h.restore(res.Path, true, vol)
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
	// Use context.Background(), not h.ctx: h.ctx has a 5-minute timeout, and
	// cleanup must still run when that timeout is exactly why the test
	// failed. A silently-failed ContainerRemove here would keep the volume
	// in use, so VolumeRemove in t.Cleanup would fail too, leaking
	// dv-backup-test-* resources.
	t.Cleanup(func() { _, _ = h.raw.ContainerRemove(context.Background(), c.ID, client.ContainerRemoveOptions{Force: true}) })

	if _, err := h.restore(res.Path, false, vol); !errors.Is(err, restore.ErrBlocked) {
		t.Fatalf("expected blocked (copy-up made the volume non-empty), got %v", err)
	}
	if out, err := h.restore(res.Path, true, vol); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.sh(vol, "cat /data/index.html") != "mine\n" || strings.Contains(h.sh(vol, "ls /data"), "50x.html") {
		t.Fatalf("volume content after forced restore: %s", h.sh(vol, "ls /data"))
	}
}
