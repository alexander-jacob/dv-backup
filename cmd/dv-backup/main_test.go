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
