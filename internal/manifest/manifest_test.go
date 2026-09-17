package manifest

import (
	"strings"
	"testing"
	"time"
)

const sample = `format_version: 1
tool: dv-backup v0.1.0
created_at: 2026-09-17T12:00:00Z
host: vm-app-01
docker_version: 29.8.1
helper_image: debian:13-slim@sha256:abc
volumes:
  - name: n8n_n8n_data
    driver: local
    driver_opts: {}
    labels:
      com.docker.compose.project: n8n
      com.docker.compose.volume: n8n_data
    size_bytes: 52428800
    archive:
      path: volumes/n8n_n8n_data.tar.zst
      size_bytes: 9834211
      sha256: 3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c
    consistent: true
    containers:
      - name: n8n-n8n-1
        image: docker.n8n.io/n8nio/n8n
        compose_service: n8n
        was_running: true
`

func TestParseSample(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.Host != "vm-app-01" || m.DockerVersion != "29.8.1" {
		t.Fatalf("header fields wrong: %+v", m)
	}
	if !m.CreatedAt.Equal(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %v", m.CreatedAt)
	}
	v, ok := m.Find("n8n_n8n_data")
	if !ok {
		t.Fatal("volume not found")
	}
	if v.Project() != "n8n" || v.Archive.SizeBytes != 9834211 || !v.Consistent {
		t.Fatalf("volume fields wrong: %+v", v)
	}
	if len(v.Containers) != 1 || v.Containers[0].ComposeService != "n8n" || !v.Containers[0].WasRunning {
		t.Fatalf("containers wrong: %+v", v.Containers)
	}
}

func TestRoundTrip(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if again.Volumes[0].Archive.SHA256 != m.Volumes[0].Archive.SHA256 || again.Volumes[0].Labels["com.docker.compose.project"] != "n8n" {
		t.Fatalf("round trip lost data: %+v", again)
	}
}

func TestRefusesUnknownFormatVersion(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(sample, "format_version: 1", "format_version: 2", 1)))
	if err == nil || !strings.Contains(err.Error(), "format_version 2") {
		t.Fatalf("expected format_version error, got %v", err)
	}
	_, err = Parse([]byte(strings.Replace(sample, "format_version: 1\n", "", 1)))
	if err == nil {
		t.Fatal("expected error for missing format_version")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "n8n_n8n_data", "my-vol.1", "0abc"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "-x", "_x", "a b", "a/b", "a\x00b", "über"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"bad name":       strings.Replace(sample, "name: n8n_n8n_data", "name: ../etc", 1),
		"path mismatch":  strings.Replace(sample, "path: volumes/n8n_n8n_data.tar.zst", "path: volumes/other.tar.zst", 1),
		"short checksum": strings.Replace(sample, "sha256: 3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c", "sha256: abc", 1),
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	dup := strings.Replace(sample, "volumes:\n", "volumes:\n  - name: n8n_n8n_data\n    archive:\n      path: volumes/n8n_n8n_data.tar.zst\n      sha256: 3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c3f1c\n", 1)
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: got %v", err)
	}
}
