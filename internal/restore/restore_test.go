package restore

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

func run(t *testing.T, f *dockerx.Fake, opts Options) (Result, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	res, err := Run(context.Background(), f, &out, &errOut, opts)
	return res, out.String(), err
}

func TestRestoreFreshHost(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	res, out, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage})
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
	_, out, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage})
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
	res, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Force: true})
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

func TestRestoreStopFailure(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local"}, []byte("OLD"))
	f.AddVolume(dockerx.Volume{Name: "app_files", Driver: "local"}, []byte("OLDFILES"))
	f.AddContainer(dockerx.Container{ID: "c1", Name: "app-db-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	f.AddContainer(dockerx.Container{ID: "c2", Name: "app-files-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_files"}}})
	f.FailStop["c2"] = errors.New("stop failed")
	res, out, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Force: true})
	if err == nil || !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(res.Untouched, ",") != "app_db,app_files" || len(res.Restored)+len(res.Failed) != 0 {
		t.Fatalf("res %+v", res)
	}
	if string(f.Data["app_db"]) != "OLD" || string(f.Data["app_files"]) != "OLDFILES" {
		t.Fatal("stop failure must not touch volume data")
	}
	if !strings.Contains(out, "untouched: app_db, app_files") || !strings.Contains(out, "retry with:") ||
		!strings.Contains(out, "dv-backup restore --force "+path+" app_db app_files") {
		t.Fatalf("summary/retry missing:\n%s", out)
	}
	cs, _ := f.ListContainers(context.Background())
	for _, c := range cs {
		if c.State != dockerx.StateRunning {
			t.Fatalf("container %s not restarted: %s", c.ID, c.State)
		}
	}
}

func TestRestoreSelectionAndUnknownName(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	if _, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Names: []string{"app_files"}}); err != nil {
		t.Fatal(err)
	}
	if f.Data["app_db"] != nil || string(f.Data["app_files"]) != "FILES" {
		t.Fatal("only app_files should be restored")
	}
	if _, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Names: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreFailureMidWay(t *testing.T) {
	path := writeArchive(t, t.TempDir(), map[string]string{"app_db": "DB", "app_files": "FILES"})
	f := dockerx.NewFake()
	f.AddVolume(dockerx.Volume{Name: "app_db", Driver: "local"}, nil)
	f.AddContainer(dockerx.Container{ID: "c", Name: "app-db-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "app_db"}}})
	f.FailHelper["tar app_db"] = errors.New("disk on fire")
	res, out, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage, Force: true})
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
	// Corrupt the first volume entry: byte 512+4 is inside the first entry's
	// zstd frame (the "DB" test volume compresses to about 15 bytes, so a
	// byte further in, e.g. 600, would fall in tar padding and the sha256
	// would not change).
	b, _ := os.ReadFile(path)
	b[512+4] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f := dockerx.NewFake()
	_, _, err := run(t, f, Options{Archive: path, Image: dockerx.DefaultImage})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("Docker must not be touched: %v", f.Calls)
	}
}

func TestRestoreRetryCommandIsExact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeArchive(t, dir, map[string]string{"app_db": "DB", "app_files": "FILES"})
	const mirror = "mirror.example:5000/library/debian:13-slim"
	f := dockerx.NewFake()
	f.Images[mirror] = mirror + "@sha256:fake"
	f.FailHelper["tar app_db"] = errors.New("disk on fire")
	_, out, err := run(t, f, Options{Archive: path, Image: mirror})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "dv-backup restore --force --image " + mirror + " '" + path + "' app_db app_files\n"
	if !strings.Contains(out, want) {
		t.Fatalf("retry command not exact, want %q in:\n%s", want, out)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"app_db":                 "app_db",
		"/var/backups/x-1.tar":   "/var/backups/x-1.tar",
		"debian:13-slim@sha256:": "debian:13-slim@sha256:",
		"my backups/x.tar":       "'my backups/x.tar'",
		"it's.tar":               `'it'\''s.tar'`,
		"$HOME/x;rm":             "'$HOME/x;rm'",
		"":                       "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
