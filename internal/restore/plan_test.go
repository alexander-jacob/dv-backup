package restore

import (
	"strings"
	"testing"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

func mv(name string) manifest.Volume {
	return manifest.Volume{Name: name, Driver: "local", DriverOpts: map[string]string{}, Labels: map[string]string{"com.docker.compose.project": "app"}}
}

func running(id string) dockerx.Container {
	return dockerx.Container{ID: id, Name: "c-" + id, State: dockerx.StateRunning}
}

func TestBuildPlanTable(t *testing.T) {
	cases := []struct {
		name       string
		state      State
		force      bool
		want       Action
		stops      int
		reasonPart string
	}{
		{"missing", State{}, false, ActionCreate, 0, "does not exist"},
		{"missing force", State{}, true, ActionCreate, 0, "does not exist"},
		{"empty idle", State{Exists: true, Empty: true}, false, ActionUnpack, 0, "empty"},
		{"empty idle force", State{Exists: true, Empty: true}, true, ActionUnpack, 0, "empty"},
		{"has data", State{Exists: true}, false, ActionBlocked, 0, "contains data"},
		{"has data force", State{Exists: true}, true, ActionOverwrite, 0, "contains data"},
		{"empty in use", State{Exists: true, Empty: true, Users: []dockerx.Container{running("a")}}, false, ActionBlocked, 0, "in use by c-a (running)"},
		{"empty in use force", State{Exists: true, Empty: true, Users: []dockerx.Container{running("a")}}, true, ActionOverwrite, 1, "in use"},
		{"data in use force", State{Exists: true, Users: []dockerx.Container{running("a"), {ID: "b", Name: "c-b", State: dockerx.StatePaused}, {ID: "x", Name: "c-x", State: dockerx.StateExited}}}, true, ActionOverwrite, 2, "c-b (paused)"},
	}
	for _, tc := range cases {
		p := BuildPlan([]manifest.Volume{mv("v")}, map[string]State{"v": tc.state}, tc.force)
		s := p.Steps[0]
		if s.Action != tc.want || len(s.Stop) != tc.stops || !strings.Contains(s.Reason, tc.reasonPart) {
			t.Errorf("%s: got %s stops=%d reason=%q", tc.name, s.Action, len(s.Stop), s.Reason)
		}
		if s.Action == ActionBlocked && !strings.Contains(s.Reason, "--force") {
			t.Errorf("%s: blocked reason must mention --force", tc.name)
		}
		if p.Blocked() != (tc.want == ActionBlocked) {
			t.Errorf("%s: Blocked() wrong", tc.name)
		}
	}
}

func TestBuildPlanWarningsAndToStop(t *testing.T) {
	existing := dockerx.Volume{Name: "v", Driver: "nfs", Options: map[string]string{"o": "addr=x"}, Labels: map[string]string{}}
	shared := running("a")
	states := map[string]State{
		"v": {Exists: true, Existing: existing, Users: []dockerx.Container{shared}},
		"w": {Exists: true, Empty: true, Users: []dockerx.Container{shared, running("b")},
			Existing: dockerx.Volume{Name: "w", Driver: "local", Options: map[string]string{}, Labels: map[string]string{"com.docker.compose.project": "app"}}},
	}
	p := BuildPlan([]manifest.Volume{mv("v"), mv("w")}, states, true)
	if len(p.Steps[0].Warnings) != 3 {
		t.Fatalf("expected driver, options and labels warnings, got %v", p.Steps[0].Warnings)
	}
	if len(p.Steps[1].Warnings) != 0 {
		t.Fatalf("no warnings expected when metadata matches, got %v", p.Steps[1].Warnings)
	}
	ids := []string{}
	for _, c := range p.ToStop() {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("ToStop = %v", ids)
	}
}
