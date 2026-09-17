package manifest

import (
	"fmt"
	"strings"
	"time"
)

// FormatBytes renders a byte count for humans. Negative means unknown.
func FormatBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// RenderREADME produces the human-readable README.md stored next to manifest.yaml.
// It is derived from the manifest and must never be parsed by tools.
func RenderREADME(m *Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dv-backup archive\n\n")
	fmt.Fprintf(&b, "- Host: %s\n- Created: %s\n- Docker: %s\n- Tool: %s\n- Helper image: %s\n\n",
		m.Host, m.CreatedAt.UTC().Format(time.RFC3339), m.DockerVersion, m.Tool, m.HelperImage)
	fmt.Fprintf(&b, "This archive is **not encrypted**. Volume data often contains secrets; store and copy it accordingly.\n\n")
	fmt.Fprintf(&b, "## Volumes\n\n| Volume | Project | Driver | Data | Compressed | Consistent | Containers |\n|---|---|---|---|---|---|---|\n")
	for _, v := range m.Volumes {
		cons := "yes"
		if !v.Consistent {
			cons = "**not consistent**"
		}
		var cs []string
		for _, c := range v.Containers {
			state := "stopped"
			if c.WasRunning {
				state = "running"
			}
			cs = append(cs, fmt.Sprintf("%s (%s)", c.Name, state))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			v.Name, v.Project(), v.Driver, FormatBytes(v.SizeBytes), FormatBytes(v.Archive.SizeBytes), cons, strings.Join(cs, ", "))
	}
	fmt.Fprintf(&b, "\n## Restore\n\n```\ndv-backup restore <this file>            # fresh host\ndv-backup restore --force <this file>    # host with existing data\ndv-backup stat --archive <this file>     # inspect\n```\n")
	return b.String()
}
