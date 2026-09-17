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

	"github.com/alexander-jacob/dv-backup/internal/version"
)

// exitError carries an exit code from a command's RunE to run().
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
	// Normalize nil args to empty non-nil slice to prevent cobra from using os.Args under go test.
	if args == nil {
		args = []string{}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRoot(ctx, stdout, stderr)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(stderr, "error:", ee.err)
		return ee.code
	}
	// Anything else came from cobra itself: unknown command, bad flag, wrong arg count.
	fmt.Fprintln(stderr, "error:", err)
	return 2
}

func newRoot(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "dv-backup",
		Short:         "Back up and restore named Docker volumes",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetContext(ctx)
	root.SetVersionTemplate("dv-backup {{.Version}}\n")
	return root
}
