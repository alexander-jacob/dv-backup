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
	id := h.container(vol, map[string]string{"com.docker.compose.service": "web"})
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
	// Pull hello-world up front and fail loudly if that fails, so a later
	// pull failure inside backup.Run (which happens before any container is
	// stopped) cannot be mistaken for the restart-after-failure behaviour
	// this test exists to exercise.
	if _, err := h.d.EnsureImage(h.ctx, "hello-world"); err != nil {
		t.Fatalf("pull hello-world: %v", err)
	}
	vol := h.volume(nil)
	id := h.container(vol, nil)
	firstStart := h.startedAt(id)
	var out, errOut strings.Builder
	// hello-world has no tar binary: the helper fails to start.
	_, err := backup.Run(h.ctx, h.d, &out, &errOut, backup.Options{OutputDir: t.TempDir(), Names: []string{vol}, Image: "hello-world"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if h.state(id) != "running" {
		t.Fatalf("container not restarted after failed backup: %s", h.state(id))
	}
	if !h.startedAt(id).After(firstStart) {
		t.Fatal("container was not restarted (StartedAt unchanged)")
	}
}

func TestPausedContainerIsStoppedAndComesBackRunning(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	id := h.container(vol, nil)
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
