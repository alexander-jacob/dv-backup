// Package manifest defines the archive manifest (manifest.yaml) and its rendering.
package manifest

import (
	"fmt"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"
)

// FormatVersion is the manifest format this build reads and writes.
const FormatVersion = 1

// LabelComposeProject is the Compose label that names a volume's project.
const LabelComposeProject = "com.docker.compose.project"

// nameRE mirrors Docker's restricted name rule. It deliberately allows
// one-character names (see spec §10.9) so that no valid Docker name is rejected.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidName reports whether name is a valid Docker volume name and therefore
// safe to use as a path component inside the archive.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// VolumePath returns the archive entry path for a volume.
func VolumePath(name string) string { return "volumes/" + name + ".tar.zst" }

// Manifest describes one archive.
type Manifest struct {
	FormatVersion int       `yaml:"format_version"`
	Tool          string    `yaml:"tool"`
	CreatedAt     time.Time `yaml:"created_at"`
	Host          string    `yaml:"host"`
	DockerVersion string    `yaml:"docker_version"`
	HelperImage   string    `yaml:"helper_image"`
	Volumes       []Volume  `yaml:"volumes"`
}

// Volume describes one backed-up volume.
type Volume struct {
	Name       string            `yaml:"name"`
	Driver     string            `yaml:"driver"`
	DriverOpts map[string]string `yaml:"driver_opts"`
	Labels     map[string]string `yaml:"labels"`
	SizeBytes  int64             `yaml:"size_bytes"`
	Archive    Entry             `yaml:"archive"`
	Consistent bool              `yaml:"consistent"`
	Containers []Container       `yaml:"containers"`
}

// Entry locates and checksums a volume's compressed stream inside the archive.
type Entry struct {
	Path      string `yaml:"path"`
	SizeBytes int64  `yaml:"size_bytes"`
	SHA256    string `yaml:"sha256"`
}

// Container records a container that used the volume at backup time.
type Container struct {
	Name           string `yaml:"name"`
	Image          string `yaml:"image"`
	ComposeService string `yaml:"compose_service,omitempty"`
	WasRunning     bool   `yaml:"was_running"`
}

// Project returns the Compose project of the volume, or "".
func (v Volume) Project() string { return v.Labels[LabelComposeProject] }

// Marshal renders the manifest as YAML.
func (m *Manifest) Marshal() ([]byte, error) { return yaml.Marshal(m) }

// Parse decodes and validates a manifest. It refuses unknown format versions.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest.yaml: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("manifest format_version %d is not supported by this dv-backup (supports %d); use a matching dv-backup release",
			m.FormatVersion, FormatVersion)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks names, paths and checksums so that archive entries can be trusted as file names.
func (m *Manifest) Validate() error {
	seen := make(map[string]bool, len(m.Volumes))
	for _, v := range m.Volumes {
		if !ValidName(v.Name) {
			return fmt.Errorf("manifest: invalid volume name %q", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("manifest: duplicate volume %q", v.Name)
		}
		seen[v.Name] = true
		if v.Archive.Path != VolumePath(v.Name) {
			return fmt.Errorf("manifest: volume %q: unexpected archive path %q", v.Name, v.Archive.Path)
		}
		if len(v.Archive.SHA256) != 64 {
			return fmt.Errorf("manifest: volume %q: invalid sha256 %q", v.Name, v.Archive.SHA256)
		}
	}
	return nil
}

// Find returns the volume with the given name.
func (m *Manifest) Find(name string) (*Volume, bool) {
	for i := range m.Volumes {
		if m.Volumes[i].Name == name {
			return &m.Volumes[i], true
		}
	}
	return nil, false
}
