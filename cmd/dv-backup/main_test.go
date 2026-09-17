package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
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

// cancelOnWrite cancels a context the first time a write contains trigger.
type cancelOnWrite struct {
	bytes.Buffer
	trigger string
	cancel  func()
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.cancel != nil && strings.Contains(string(p), w.trigger) {
		w.cancel()
		w.cancel = nil
	}
	return n, err
}

func TestInterruptedRunReportsInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		args          func(dir, archive string) []string
	}{
		{"backup", "backing up volume v2", func(dir, _ string) []string { return []string{"backup", "-o", dir, "v1", "v2"} }},
		{"restore", "restoring volume v2", func(_, archive string) []string { return []string{"restore", "--force", archive} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := dockerx.NewFake()
			f.AddVolume(dockerx.Volume{Name: "v1", Driver: "local"}, []byte("ONE"))
			f.AddVolume(dockerx.Volume{Name: "v2", Driver: "local"}, []byte("TWO"))
			f.AddContainer(dockerx.Container{ID: "c", Name: "app-1", State: dockerx.StateRunning, Mounts: []dockerx.Mount{{Type: "volume", Name: "v1"}}})
			code, out, _ := runWith(t, f, "backup", "-o", t.TempDir(), "v1", "v2")
			if code != 0 {
				t.Fatal(out)
			}
			archive, _, _ := strings.Cut(out[strings.LastIndex(out, "wrote ")+len("wrote "):], "\n")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stdout := &cancelOnWrite{trigger: tc.trigger, cancel: cancel}
			var stderr bytes.Buffer
			a := &app{stdout: stdout, stderr: &stderr, connect: func() (dockerx.Docker, error) { return f, nil }}
			code = a.run(ctx, tc.args(t.TempDir(), archive))
			if code != 1 {
				t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "error: interrupted") || strings.Contains(stderr.String(), "context canceled") {
				t.Fatalf("stderr = %q, want a clear interrupted error", stderr.String())
			}
			if !strings.Contains(stdout.String(), "starting container app-1") {
				t.Fatalf("restart outcome not printed:\n%s", stdout.String())
			}
		})
	}
}
