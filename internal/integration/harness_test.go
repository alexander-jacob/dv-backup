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

func (h *harness) populate(vol string)        { h.sh(vol, populateScript) }
func (h *harness) listing(vol string) string { return h.sh(vol, inspectScript) }

// container creates and starts a sleeping container that mounts vol.
func (h *harness) container(vol string, labels map[string]string) string {
	name := h.name()
	if !strings.HasPrefix(name, prefix) {
		h.t.Fatal("bad name")
	}
	res, err := h.raw.ContainerCreate(h.ctx, client.ContainerCreateOptions{
		Name:       name,
		Config:     &container.Config{Image: dockerx.DefaultImage, Cmd: []string{"sleep", "600"}, Labels: labels},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: "/mnt/vol"}}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		_, _ = h.raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := h.raw.ContainerStart(h.ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		h.t.Fatal(err)
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

func (h *harness) restore(path string, force bool, vols ...string) (string, error) {
	if len(vols) == 0 {
		h.t.Fatal("integration tests must always pass explicit volume names to restore")
	}
	var out, errOut bytes.Buffer
	_, err := restore.Run(h.ctx, h.d, &out, &errOut, restore.Options{Archive: path, Names: vols, Force: force, Image: dockerx.DefaultImage})
	return out.String() + errOut.String(), err
}
