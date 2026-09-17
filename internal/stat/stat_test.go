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
	f.AddContainer(dockerx.Container{ID: "h-running", Name: "dv-backup-helper-active", State: dockerx.StateRunning, Labels: map[string]string{dockerx.LabelHelper: "true"}, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
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
	if strings.Contains(s, "dv-backup-helper-active") {
		t.Errorf("running helper must not be listed as stray:\n%s", s)
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
