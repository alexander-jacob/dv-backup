// Package selection filters volumes by name and Compose project.
package selection

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsAnonymous reports whether a volume was created anonymously by Docker.
func IsAnonymous(v dockerx.Volume) bool {
	if _, ok := v.Labels[dockerx.LabelAnonymous]; ok {
		return true
	}
	return hex64.MatchString(v.Name)
}

// Named returns the non-anonymous volumes sorted by name.
func Named(vols []dockerx.Volume) []dockerx.Volume {
	out := make([]dockerx.Volume, 0, len(vols))
	for _, v := range vols {
		if !IsAnonymous(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Filter applies the union of explicit names and Compose projects. With no
// filters every item is returned. A name or project that matches nothing is
// an error, as is an empty result.
func Filter[T any](items []T, name func(T) string, project func(T) string, names, projects []string) ([]T, error) {
	if len(names) == 0 && len(projects) == 0 {
		if len(items) == 0 {
			return nil, fmt.Errorf("no named volumes found")
		}
		return items, nil
	}
	wantName := map[string]bool{}
	for _, n := range names {
		wantName[n] = false
	}
	wantProject := map[string]bool{}
	for _, p := range projects {
		wantProject[p] = false
	}
	var out []T
	for _, it := range items {
		_, byName := wantName[name(it)]
		_, byProject := wantProject[project(it)]
		if byName {
			wantName[name(it)] = true
		}
		if byProject {
			wantProject[project(it)] = true
		}
		if byName || byProject {
			out = append(out, it)
		}
	}
	var missing []string
	for n, seen := range wantName {
		if !seen {
			missing = append(missing, "volume "+n)
		}
	}
	for p, seen := range wantProject {
		if !seen {
			missing = append(missing, "project "+p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("not found: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Volumes selects named host volumes by names and projects.
func Volumes(vols []dockerx.Volume, names, projects []string) ([]dockerx.Volume, error) {
	return Filter(Named(vols),
		func(v dockerx.Volume) string { return v.Name },
		func(v dockerx.Volume) string { return v.Labels[dockerx.LabelComposeProject] },
		names, projects)
}
