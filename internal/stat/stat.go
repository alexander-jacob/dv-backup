// Package stat renders what exists on a host and what an archive contains.
package stat

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
	"github.com/alexander-jacob/dv-backup/internal/selection"
)

var skippedBindPrefixes = []string{"/var/run", "/run", "/dev", "/proc", "/sys"}

// Host prints the named volumes of the daemon with sizes and users, then warnings.
func Host(ctx context.Context, d dockerx.Docker, out io.Writer) error {
	info, err := d.Info(ctx)
	if err != nil {
		return err
	}
	vols, err := d.ListVolumes(ctx)
	if err != nil {
		return err
	}
	sizes, err := d.VolumeSizes(ctx)
	if err != nil {
		return err
	}
	containers, err := d.ListContainers(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "host %s, Docker %s\n\n", info.Name, info.ServerVersion)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VOLUME\tPROJECT\tDRIVER\tSIZE\tCONTAINERS")
	for _, v := range selection.Named(vols) {
		size := int64(-1)
		if s, ok := sizes[v.Name]; ok {
			size = s
		}
		var users []string
		for _, c := range containers {
			if !c.IsHelper() && c.UsesVolume(v.Name) {
				users = append(users, fmt.Sprintf("%s (%s)", c.Name, c.State))
			}
		}
		sort.Strings(users)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, v.Labels[dockerx.LabelComposeProject], v.Driver, manifest.FormatBytes(size), strings.Join(users, ", "))
	}
	tw.Flush()

	var warnings []string
	for _, v := range vols {
		if selection.IsAnonymous(v) {
			warnings = append(warnings, fmt.Sprintf("anonymous volume %s is not backed up", v.Name))
		}
	}
	var stray []string
	for _, c := range containers {
		if c.IsHelper() {
			stray = append(stray, c.Name)
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "bind" && m.RW && !skipBind(m.Source) {
				warnings = append(warnings, fmt.Sprintf("container %s has writable bind mount %s -> %s; bind mounts are not backed up", c.Name, m.Source, m.Destination))
			}
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		warnings = append(warnings, fmt.Sprintf("stray helper container(s) %s from an interrupted run; remove with: docker rm -f $(docker ps -aq --filter label=%s=true)", strings.Join(stray, ", "), dockerx.LabelHelper))
	}
	if len(warnings) > 0 {
		fmt.Fprintln(out, "\nwarnings:")
		for _, w := range warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	return nil
}

func skipBind(source string) bool {
	for _, p := range skippedBindPrefixes {
		if source == p || strings.HasPrefix(source, p+"/") {
			return true
		}
	}
	return false
}

// Archive prints the manifest of an archive without reading volume data.
func Archive(path string, out io.Writer) error {
	r, err := archive.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	m := r.Manifest()
	fmt.Fprintf(out, "archive %s\n  host: %s\n  created: %s\n  docker: %s\n  tool: %s\n  helper image: %s\n\n",
		path, m.Host, m.CreatedAt.UTC().Format(time.RFC3339), m.DockerVersion, m.Tool, m.HelperImage)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VOLUME\tPROJECT\tDATA\tCOMPRESSED\tCONSISTENT\tSHA256")
	var warnings []string
	for _, v := range m.Volumes {
		cons := "yes"
		if !v.Consistent {
			cons = "no"
			warnings = append(warnings, fmt.Sprintf("volume %s is not consistent (backed up while in use)", v.Name))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Project(), manifest.FormatBytes(v.SizeBytes), manifest.FormatBytes(v.Archive.SizeBytes), cons, v.Archive.SHA256)
	}
	tw.Flush()
	if len(warnings) > 0 {
		fmt.Fprintln(out, "\nwarnings:")
		for _, w := range warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	return nil
}
