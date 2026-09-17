// Package backup archives selected volumes into one archive file.
package backup

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
	"github.com/alexander-jacob/dv-backup/internal/selection"
	"github.com/alexander-jacob/dv-backup/internal/stopper"
	"github.com/alexander-jacob/dv-backup/internal/version"
)

// Options controls a backup run.
type Options struct {
	OutputDir string
	Names     []string
	Projects  []string
	NoStop    bool
	Image     string
	Now       func() time.Time // nil = time.Now
}

// Result reports the archive written and any containers that failed to restart.
type Result struct {
	Path          string
	RestartErrors []error
}

// Run performs a backup. Containers stopped by Run are always restarted, even
// on error or cancellation; the archive is removed on error.
func Run(ctx context.Context, d dockerx.Docker, out, errOut io.Writer, opts Options) (res Result, err error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// Preflight: everything that can fail before any state changes.
	info, err := d.Info(ctx)
	if err != nil {
		return res, err
	}
	digest, err := d.EnsureImage(ctx, opts.Image)
	if err != nil {
		return res, err
	}
	vols, err := d.ListVolumes(ctx)
	if err != nil {
		return res, err
	}
	selected, err := selection.Volumes(vols, opts.Names, opts.Projects)
	if err != nil {
		return res, err
	}
	containers, err := d.ListContainers(ctx)
	if err != nil {
		return res, err
	}
	if sizes, err := d.VolumeSizes(ctx); err == nil {
		warnFreeSpace(errOut, opts.OutputDir, selected, sizes)
	}
	w, err := archive.NewWriter(opts.OutputDir, info.Name, now())
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			if aerr := w.Abort(); aerr != nil {
				fmt.Fprintf(errOut, "warning: %v\n", aerr)
			}
		}
	}()

	names := make([]string, len(selected))
	for i, v := range selected {
		names[i] = v.Name
	}
	st := stopper.New(d, out)
	defer func() {
		res.RestartErrors = st.Restart(context.WithoutCancel(ctx))
	}()
	if !opts.NoStop {
		if err = st.Stop(ctx, stopper.UsersOf(names, containers)); err != nil {
			return res, err
		}
	}

	m := &manifest.Manifest{
		FormatVersion: manifest.FormatVersion, Tool: version.Tool(), CreatedAt: now().UTC(),
		Host: info.Name, DockerVersion: info.ServerVersion, HelperImage: digest,
	}
	for _, v := range selected {
		fmt.Fprintf(out, "backing up volume %s\n", v.Name)
		start := time.Now()
		mv, verr := backupVolume(ctx, d, w, opts, v, containers)
		if verr != nil {
			err = verr
			return res, err
		}
		if !mv.Consistent {
			fmt.Fprintf(errOut, "warning: volume %s was backed up while in use; marked inconsistent\n", v.Name)
		}
		fmt.Fprintf(out, "  %s: %s data, %s compressed, %s\n", v.Name,
			manifest.FormatBytes(mv.SizeBytes), manifest.FormatBytes(mv.Archive.SizeBytes), time.Since(start).Round(time.Millisecond))
		m.Volumes = append(m.Volumes, mv)
	}
	if err = w.Finish(m); err != nil {
		return res, err
	}
	res.Path = w.Path()
	fmt.Fprintf(out, "wrote %s\n", res.Path)
	return res, nil
}

func backupVolume(ctx context.Context, d dockerx.Docker, w *archive.Writer, opts Options, v dockerx.Volume, containers []dockerx.Container) (manifest.Volume, error) {
	users := containersUsing(v.Name, containers)
	anyInUse := false
	mv := manifest.Volume{Name: v.Name, Driver: v.Driver, DriverOpts: orEmpty(v.Options), Labels: orEmpty(v.Labels), Consistent: true}
	for _, c := range users {
		mv.Containers = append(mv.Containers, manifest.Container{Name: c.Name, Image: c.Image, ComposeService: c.ComposeService(), WasRunning: c.InUse()})
		anyInUse = anyInUse || c.InUse()
	}

	pr, pw := io.Pipe()
	type outcome struct {
		res dockerx.HelperResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := d.RunHelper(ctx, dockerx.BackupHelper(opts.Image, v.Name, pw))
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
		done <- outcome{res, err}
	}()
	entry, uncompressed, addErr := w.AddVolume(v.Name, pr)
	if addErr != nil {
		pr.CloseWithError(addErr) // unblocks the helper's writes
	}
	o := <-done
	if o.err != nil {
		return mv, fmt.Errorf("volume %s: %w", v.Name, o.err)
	}
	if addErr != nil {
		return mv, addErr
	}
	switch {
	case o.res.ExitCode == 0:
	case o.res.ExitCode == 1 && opts.NoStop:
		mv.Consistent = false
	default:
		return mv, fmt.Errorf("volume %s: tar exit %d: %s", v.Name, o.res.ExitCode, o.res.Stderr)
	}
	if opts.NoStop && anyInUse {
		mv.Consistent = false
	}
	mv.SizeBytes = uncompressed
	mv.Archive = entry
	return mv, nil
}

func containersUsing(volume string, containers []dockerx.Container) []dockerx.Container {
	var out []dockerx.Container
	for _, c := range containers {
		if !c.IsHelper() && c.UsesVolume(volume) {
			out = append(out, c)
		}
	}
	return out
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// warnFreeSpace prints a warning if the selected volumes' disk usage exceeds
// the free space in dir. Sizes are approximate (disk usage, not tar size).
func warnFreeSpace(errOut io.Writer, dir string, selected []dockerx.Volume, sizes map[string]int64) {
	var total int64
	for _, v := range selected {
		if s, ok := sizes[v.Name]; ok && s > 0 {
			total += s
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return
	}
	free := int64(st.Bavail) * st.Bsize // Bsize is int64 on linux/amd64 and linux/arm64
	if total > free {
		fmt.Fprintf(errOut, "warning: selected volumes use %s but %s has only %s free\n",
			manifest.FormatBytes(total), dir, manifest.FormatBytes(free))
	}
}
