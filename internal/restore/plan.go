// Package restore verifies an archive, plans changes and applies them.
package restore

import (
	"fmt"
	"maps"
	"strings"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

// Action is what the plan does with one volume.
type Action int

// Plan actions.
const (
	ActionCreate Action = iota
	ActionUnpack
	ActionOverwrite
	ActionBlocked
)

func (a Action) String() string {
	return [...]string{"create", "unpack", "overwrite", "blocked"}[a]
}

// State is what exists on the host for one volume.
type State struct {
	Exists   bool
	Empty    bool
	Users    []dockerx.Container // containers mounting the volume, any state, helpers excluded
	Existing dockerx.Volume
}

// Step is the planned action for one volume.
type Step struct {
	Volume   manifest.Volume
	Action   Action
	Reason   string
	Stop     []dockerx.Container
	Warnings []string
}

// Plan is the full set of steps in manifest order.
type Plan struct {
	Steps []Step
}

// Blocked reports whether any step is blocked.
func (p Plan) Blocked() bool {
	for _, s := range p.Steps {
		if s.Action == ActionBlocked {
			return true
		}
	}
	return false
}

// ToStop lists every container the plan stops, once each, in step order.
func (p Plan) ToStop() []dockerx.Container {
	seen := map[string]bool{}
	var out []dockerx.Container
	for _, s := range p.Steps {
		for _, c := range s.Stop {
			if !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// BuildPlan decides per volume what to do. It is a pure function of the
// manifest volumes, the observed host state and the --force flag (spec §7.3).
func BuildPlan(vols []manifest.Volume, states map[string]State, force bool) Plan {
	var p Plan
	for _, v := range vols {
		st := states[v.Name]
		step := Step{Volume: v}
		var inUse []dockerx.Container
		var users []string
		for _, c := range st.Users {
			if c.InUse() {
				inUse = append(inUse, c)
				users = append(users, fmt.Sprintf("%s (%s)", c.Name, c.State))
			}
		}
		switch {
		case !st.Exists:
			step.Action, step.Reason = ActionCreate, "volume does not exist"
		case st.Empty && len(inUse) == 0:
			step.Action, step.Reason = ActionUnpack, "volume exists and is empty"
		default:
			var why []string
			if !st.Empty {
				why = append(why, "contains data")
			}
			if len(inUse) > 0 {
				why = append(why, "in use by "+strings.Join(users, ", "))
			}
			step.Reason = strings.Join(why, "; ")
			if force {
				step.Action, step.Stop = ActionOverwrite, inUse
			} else {
				step.Action = ActionBlocked
				step.Reason += "; use --force to overwrite"
			}
		}
		if st.Exists {
			driver := v.Driver
			if driver == "" {
				driver = "local"
			}
			if st.Existing.Driver != driver {
				step.Warnings = append(step.Warnings, fmt.Sprintf("driver is %q, archive has %q (cannot be changed)", st.Existing.Driver, driver))
			}
			if !maps.Equal(st.Existing.Options, v.DriverOpts) && (len(st.Existing.Options) > 0 || len(v.DriverOpts) > 0) {
				step.Warnings = append(step.Warnings, "driver options differ from the archive (cannot be changed)")
			}
			if !maps.Equal(st.Existing.Labels, v.Labels) && (len(st.Existing.Labels) > 0 || len(v.Labels) > 0) {
				step.Warnings = append(step.Warnings, "labels differ from the archive (cannot be changed; Compose may warn that it did not create this volume)")
			}
		}
		p.Steps = append(p.Steps, step)
	}
	return p
}
