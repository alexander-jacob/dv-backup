// Command dv-backup backs up and restores named Docker volumes.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alexander-jacob/dv-backup/internal/backup"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
	"github.com/alexander-jacob/dv-backup/internal/restore"
	"github.com/alexander-jacob/dv-backup/internal/stat"
	"github.com/alexander-jacob/dv-backup/internal/version"
)

const longHelp = `dv-backup backs up named Docker volumes into one archive file and restores
them. Volume data is read and written through a short-lived helper container
running GNU tar (image: --image / DV_BACKUP_IMAGE, default ` + dockerx.DefaultImage + `),
so ownership, modes, links, sparse files and xattrs are preserved.

By default backup stops every container that uses a selected volume, archives
the volumes and restarts exactly those containers afterwards, in the order they
were originally started. Archives are not encrypted.

Restore flows for a host rebuilt by Terraform and deployed by a pipeline:

  before deploy:   taint -> apply -> scp archive -> dv-backup restore -> run pipeline
                   Volumes are created with the driver and labels from the archive,
                   so Compose adopts them.

  after deploy:    taint -> apply -> run pipeline (or compose up --no-start)
                   -> scp archive -> dv-backup restore --force -> start stack
                   --force is normally required here: Docker copies image content
                   into new volumes when the container is *created*, and database
                   images initialise their data directory on first start, so the
                   volumes are no longer empty.

Without --force, restore refuses to touch a volume that contains data or is in
use (running, paused or restarting container) and changes nothing at all.

Exit codes: 0 ok, 1 error, 2 usage, 3 finished but a stopped container could
not be restarted (start it by hand: see the output).
`

type app struct {
	stdout, stderr io.Writer
	connect        func() (dockerx.Docker, error)
	image          string
}

// exitError carries an exit code from a command to run().
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func fail(code int, err error) error { return &exitError{code: code, err: err} }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{stdout: stdout, stderr: stderr, connect: func() (dockerx.Docker, error) { return dockerx.Connect() }}
	return a.run(ctx, args)
}

func (a *app) run(ctx context.Context, args []string) int {
	// Normalize nil args to an empty non-nil slice: cobra falls back to
	// os.Args[1:] when SetArgs is given nil, which would break tests.
	if args == nil {
		args = []string{}
	}
	root := a.newRoot(ctx)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(a.stderr, "error:", ee.err)
		return ee.code
	}
	// Anything else came from cobra itself: unknown command, bad flag, wrong arg count.
	fmt.Fprintln(a.stderr, "error:", err)
	return 2
}

func (a *app) docker() (dockerx.Docker, error) {
	d, err := a.connect()
	if err != nil {
		return nil, fail(1, err)
	}
	return d, nil
}

func (a *app) newRoot(ctx context.Context) *cobra.Command {
	root := &cobra.Command{
		Use:           "dv-backup",
		Short:         "Back up and restore named Docker volumes",
		Long:          longHelp,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetContext(ctx)
	root.SetVersionTemplate("dv-backup {{.Version}}\n")
	defaultImage := os.Getenv("DV_BACKUP_IMAGE")
	if defaultImage == "" {
		defaultImage = dockerx.DefaultImage
	}
	root.PersistentFlags().StringVar(&a.image, "image", defaultImage, "helper image with GNU tar (env DV_BACKUP_IMAGE)")
	root.AddCommand(a.statCmd(), a.backupCmd(), a.restoreCmd())
	return root
}

func (a *app) statCmd() *cobra.Command {
	var archivePath string
	cmd := &cobra.Command{
		Use:   "stat",
		Short: "Show named volumes on this host, or the contents of an archive",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if archivePath != "" {
				if err := stat.Archive(archivePath, a.stdout); err != nil {
					return fail(1, err)
				}
				return nil
			}
			d, err := a.docker()
			if err != nil {
				return err
			}
			defer d.Close()
			if err := stat.Host(cmd.Context(), d, a.stdout); err != nil {
				return fail(1, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&archivePath, "archive", "", "show the manifest of this archive instead of the host")
	return cmd
}

func (a *app) backupCmd() *cobra.Command {
	opts := backup.Options{}
	cmd := &cobra.Command{
		Use:     "backup [volume...]",
		Short:   "Back up all named volumes, or only the listed ones",
		Example: "  dv-backup backup -o /var/backups\n  dv-backup backup -p n8n -p grafana\n  dv-backup backup --no-stop n8n_n8n_data",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.docker()
			if err != nil {
				return err
			}
			defer d.Close()
			opts.Names, opts.Image = args, a.image
			res, err := backup.Run(cmd.Context(), d, a.stdout, a.stderr, opts)
			return a.finish(res.RestartErrors, err)
		},
	}
	cmd.Flags().StringVarP(&opts.OutputDir, "output", "o", ".", "output directory")
	cmd.Flags().StringArrayVarP(&opts.Projects, "project", "p", nil, "only volumes of this Compose project (repeatable)")
	cmd.Flags().BoolVar(&opts.NoStop, "no-stop", false, "do not stop containers; affected volumes are marked inconsistent")
	return cmd
}

func (a *app) restoreCmd() *cobra.Command {
	opts := restore.Options{}
	cmd := &cobra.Command{
		Use:     "restore <file> [volume...]",
		Short:   "Restore all volumes from an archive, or only the listed ones",
		Example: "  dv-backup restore dv-backup-vm-app-01-20260917T120000Z.tar\n  dv-backup restore --force backup.tar\n  dv-backup restore --verify-only backup.tar\n  dv-backup restore -p n8n backup.tar",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.VerifyOnly && opts.Force {
				return errors.New("--verify-only and --force cannot be combined")
			}
			opts.Archive, opts.Names, opts.Image = args[0], args[1:], a.image
			var d dockerx.Docker
			if !opts.VerifyOnly {
				var err error
				if d, err = a.docker(); err != nil {
					return err
				}
				defer d.Close()
			}
			res, err := restore.Run(cmd.Context(), d, a.stdout, a.stderr, opts)
			return a.finish(res.RestartErrors, err)
		},
	}
	cmd.Flags().StringArrayVarP(&opts.Projects, "project", "p", nil, "only volumes of this Compose project (repeatable)")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "overwrite volumes that contain data or are in use; stops and restarts their containers")
	cmd.Flags().BoolVar(&opts.VerifyOnly, "verify-only", false, "verify manifest and checksums, change nothing (needs no Docker)")
	return cmd
}

// finish maps an operation outcome to an exit code (spec §7.4).
func (a *app) finish(restartErrs []error, err error) error {
	for _, e := range restartErrs {
		fmt.Fprintln(a.stderr, "error:", e)
	}
	if err != nil {
		return fail(1, err)
	}
	if len(restartErrs) > 0 {
		return fail(3, fmt.Errorf("%d container(s) could not be restarted; start them by hand", len(restartErrs)))
	}
	return nil
}
