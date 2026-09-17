package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
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

func TestBackupCancelledBeforeStart(t *testing.T) {
	f, opts := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, f, &bytes.Buffer{}, &bytes.Buffer{}, opts)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("nothing should have been touched: %v", f.Calls)
	}
	entries, _ := os.ReadDir(opts.OutputDir)
	if len(entries) != 0 {
		t.Fatalf("output dir not clean: %v", entries)
	}
}

// cancelOnWrite calls cancel the first time a write to it contains trigger,
// after forwarding the write to the wrapped writer. It lets a test cancel a
// context from inside a running backup, at a precise point in its output.
type cancelOnWrite struct {
	io.Writer
	trigger string
	cancel  func()
	fired   bool
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if !w.fired && strings.Contains(string(p), w.trigger) {
		w.fired = true
		w.cancel()
	}
	return n, err
}

func TestBackupCancelledMidRunStillRestarts(t *testing.T) {
	f, opts := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	cw := &cancelOnWrite{Writer: &out, trigger: "backing up volume app_files", cancel: cancel}
	_, err := Run(ctx, f, cw, &bytes.Buffer{}, opts)
	if err == nil {
		t.Fatal("expected error from cancellation mid-run")
	}
	calls := strings.Join(f.Calls, " ")
	stopAt := strings.Index(calls, "stop db")
	startAt := strings.Index(calls, "start db")
	if stopAt < 0 || startAt < 0 || stopAt > startAt {
		t.Fatalf("expected the db container stopped then restarted, in that order: %v", f.Calls)
	}
	entries, _ := os.ReadDir(opts.OutputDir)
	if len(entries) != 0 {
		t.Fatalf("output dir not clean (no .tar, .partial or temp files expected): %v", entries)
	}
}
