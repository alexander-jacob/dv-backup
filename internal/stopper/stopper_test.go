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
