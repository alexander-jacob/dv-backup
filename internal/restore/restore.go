package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/manifest"
	"github.com/alexander-jacob/dv-backup/internal/selection"
	"github.com/alexander-jacob/dv-backup/internal/stopper"
)

// ErrBlocked is returned when the plan contains a blocked volume; nothing was changed.
var ErrBlocked = errors.New("restore blocked; nothing changed (see plan above)")

// Options controls a restore run.
type Options struct {
	Archive    string
	Names      []string
	Projects   []string
	Force      bool
	VerifyOnly bool
	Image      string
}

// Result reports what happened per volume.
type Result struct {
	Restored      []string
	Failed        []string
	Untouched     []string
	RestartErrors []error
}

// Run verifies, plans and executes a restore. With VerifyOnly, d may be nil.
func Run(ctx context.Context, d dockerx.Docker, out, errOut io.Writer, opts Options) (res Result, err error) {
	r, err := archive.Open(opts.Archive)
	if err != nil {
		return res, err
	}
	defer r.Close()
	selected, err := selection.Filter(r.Manifest().Volumes,
		func(v manifest.Volume) string { return v.Name },
		func(v manifest.Volume) string { return v.Project() },
		opts.Names, opts.Projects)
	if err != nil {
		return res, err
	}
	for _, v := range selected {
		fmt.Fprintf(out, "verifying %s\n", v.Name)
		if err := r.Verify(v.Name); err != nil {
			return res, err
		}
	}
	if opts.VerifyOnly {
		fmt.Fprintf(out, "archive OK: %d volume(s) verified\n", len(selected))
		return res, nil
	}

	if _, err := d.EnsureImage(ctx, opts.Image); err != nil {
		return res, err
	}
	containers, err := d.ListContainers(ctx)
	if err != nil {
		return res, err
	}
	states := map[string]State{}
	for _, v := range selected {
		st, err := observe(ctx, d, opts.Image, v.Name, containers)
		if err != nil {
			return res, err
		}
		states[v.Name] = st
	}
	plan := BuildPlan(selected, states, opts.Force)
	PrintPlan(out, plan)
	for _, s := range plan.Steps {
		for _, w := range s.Warnings {
			fmt.Fprintf(errOut, "warning: volume %s: %s\n", s.Volume.Name, w)
		}
	}
	if plan.Blocked() {
		return res, ErrBlocked
	}

	st := stopper.New(d, out)
	defer func() {
		res.RestartErrors = st.Restart(context.WithoutCancel(ctx))
	}()
	if err = st.Stop(ctx, plan.ToStop()); err != nil {
		res.Untouched = names(plan.Steps)
		printOutcome(out, opts.Archive, res)
		return res, err
	}

	for i, s := range plan.Steps {
		fmt.Fprintf(out, "restoring volume %s (%s)\n", s.Volume.Name, s.Action)
		if err = apply(ctx, d, r, opts.Image, s); err != nil {
			res.Failed = []string{s.Volume.Name}
			res.Untouched = names(plan.Steps[i+1:])
			printOutcome(out, opts.Archive, res)
			return res, err
		}
		res.Restored = append(res.Restored, s.Volume.Name)
		fmt.Fprintf(out, "restored %s\n", s.Volume.Name)
	}
	return res, nil
}

// printOutcome prints the per-volume restored/failed/untouched summary and
// the exact command to retry the failed and untouched volumes with --force.
// Called whenever a restore stops early, whether the failure was stopping a
// container or applying a volume.
func printOutcome(out io.Writer, archivePath string, res Result) {
	fmt.Fprintf(out, "\nrestored:  %s\nfailed:    %s\nuntouched: %s\n", orNone(res.Restored), orNone(res.Failed), orNone(res.Untouched))
	retry := append(append([]string{}, res.Failed...), res.Untouched...)
	fmt.Fprintf(out, "retry with:\n  dv-backup restore --force %s %s\n", archivePath, strings.Join(retry, " "))
}

// observe gathers the host state of one volume.
func observe(ctx context.Context, d dockerx.Docker, image, name string, containers []dockerx.Container) (State, error) {
	existing, exists, err := d.InspectVolume(ctx, name)
	if err != nil {
		return State{}, err
	}
	st := State{Exists: exists, Existing: existing}
	for _, c := range containers {
		if !c.IsHelper() && c.UsesVolume(name) {
			st.Users = append(st.Users, c)
		}
	}
	if exists {
		var buf bytes.Buffer
		hr, err := d.RunHelper(ctx, dockerx.EmptyCheckHelper(image, name, &buf))
		if err != nil {
			return State{}, fmt.Errorf("check volume %s: %w", name, err)
		}
		if hr.ExitCode != 0 {
			return State{}, fmt.Errorf("check volume %s: find exit %d: %s", name, hr.ExitCode, hr.Stderr)
		}
		st.Empty = strings.TrimSpace(buf.String()) == ""
	}
	return st, nil
}

// apply executes one step. Containers are already stopped.
func apply(ctx context.Context, d dockerx.Docker, r *archive.Reader, image string, s Step) error {
	v := s.Volume
	switch s.Action {
	case ActionCreate:
		driver := v.Driver
		if driver == "" {
			driver = "local"
		}
		if err := d.CreateVolume(ctx, dockerx.Volume{Name: v.Name, Driver: driver, Options: v.DriverOpts, Labels: v.Labels}); err != nil {
			return err
		}
	case ActionOverwrite:
		hr, err := d.RunHelper(ctx, dockerx.ClearHelper(image, v.Name))
		if err != nil {
			return fmt.Errorf("clear volume %s: %w", v.Name, err)
		}
		if hr.ExitCode != 0 {
			return fmt.Errorf("clear volume %s: find exit %d: %s", v.Name, hr.ExitCode, hr.Stderr)
		}
	case ActionUnpack:
	case ActionBlocked:
		return ErrBlocked
	}
	rc, err := r.OpenVolume(v.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	hr, err := d.RunHelper(ctx, dockerx.RestoreHelper(image, v.Name, rc))
	if err != nil {
		return fmt.Errorf("unpack volume %s: %w", v.Name, err)
	}
	if hr.ExitCode != 0 {
		return fmt.Errorf("unpack volume %s: tar exit %d: %s", v.Name, hr.ExitCode, hr.Stderr)
	}
	return nil
}

// PrintPlan renders the plan as a table.
func PrintPlan(out io.Writer, p Plan) {
	fmt.Fprintln(out, "\nplan:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VOLUME\tSTATE\tACTION\tREASON")
	for _, s := range p.Steps {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Volume.Name, s.State, s.Action, s.Reason)
	}
	tw.Flush()
	fmt.Fprintln(out)
}

func names(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Volume.Name)
	}
	return out
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}
