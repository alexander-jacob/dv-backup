package selection

import (
	"strings"
	"testing"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

func vol(name, project string) dockerx.Volume {
	v := dockerx.Volume{Name: name, Driver: "local", Labels: map[string]string{}}
	if project != "" {
		v.Labels[dockerx.LabelComposeProject] = project
	}
	return v
}

func names(vs []dockerx.Volume) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return strings.Join(out, ",")
}

func TestIsAnonymous(t *testing.T) {
	labelled := dockerx.Volume{Name: "named", Labels: map[string]string{dockerx.LabelAnonymous: ""}}
	hexName := dockerx.Volume{Name: strings.Repeat("ab", 32)}
	if !IsAnonymous(labelled) || !IsAnonymous(hexName) {
		t.Fatal("anonymous volumes not detected")
	}
	if IsAnonymous(vol("app_data", "app")) || IsAnonymous(dockerx.Volume{Name: strings.Repeat("ab", 31)}) {
		t.Fatal("named volume classified anonymous")
	}
}

func TestVolumesFilters(t *testing.T) {
	all := []dockerx.Volume{vol("z_db", "z"), vol("a_data", "a"), vol("a_cache", "a"), vol("plain", ""),
		{Name: strings.Repeat("0", 64), Labels: map[string]string{dockerx.LabelAnonymous: ""}}}

	got, err := Volumes(all, nil, nil)
	if err != nil || names(got) != "a_cache,a_data,plain,z_db" {
		t.Fatalf("no filter: %s %v", names(got), err)
	}
	got, err = Volumes(all, []string{"plain"}, []string{"a"})
	if err != nil || names(got) != "a_cache,a_data,plain" {
		t.Fatalf("union: %s %v", names(got), err)
	}
	got, err = Volumes(all, []string{"z_db", "z_db"}, nil)
	if err != nil || names(got) != "z_db" {
		t.Fatalf("dedupe: %s %v", names(got), err)
	}
	if _, err = Volumes(all, []string{"nope"}, nil); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown name: %v", err)
	}
	if _, err = Volumes(all, nil, []string{"ghost"}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err = Volumes(all, []string{strings.Repeat("0", 64)}, nil); err == nil {
		t.Fatal("anonymous volume must not be selectable")
	}
	if _, err = Volumes(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "no named volumes") {
		t.Fatalf("empty host: %v", err)
	}
}
