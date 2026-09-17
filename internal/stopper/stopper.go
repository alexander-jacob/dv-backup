// Package stopper stops containers and restarts exactly those it stopped.
package stopper

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

type entry struct {
	c         dockerx.Container
	startedAt time.Time
}

// Stopper remembers which containers it stopped.
type Stopper struct {
	d       dockerx.Docker
	out     io.Writer
	stopped []entry
}

// New returns a Stopper writing progress lines to out.
func New(d dockerx.Docker, out io.Writer) *Stopper { return &Stopper{d: d, out: out} }

// UsersOf returns in-use, non-helper containers that mount any of the volumes,
// deduplicated and sorted by name.
func UsersOf(volumes []string, containers []dockerx.Container) []dockerx.Container {
	seen := map[string]bool{}
	var out []dockerx.Container
	for _, c := range containers {
		if !c.InUse() || c.IsHelper() || seen[c.ID] {
			continue
		}
		for _, v := range volumes {
			if c.UsesVolume(v) {
				seen[c.ID] = true
				out = append(out, c)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Stop stops every in-use container once, in the given order, and records it.
// It returns at the first failure; containers stopped so far stay recorded.
func (s *Stopper) Stop(ctx context.Context, containers []dockerx.Container) error {
	done := map[string]bool{}
	for _, e := range s.stopped {
		done[e.c.ID] = true
	}
	for _, c := range containers {
		if !c.InUse() || done[c.ID] {
			continue
		}
		startedAt, err := s.d.StartedAt(ctx, c.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(s.out, "stopping container %s (%s)\n", c.Name, c.State)
		if err := s.d.StopContainer(ctx, c.ID); err != nil {
			return fmt.Errorf("stop container %s: %w", c.Name, err)
		}
		done[c.ID] = true
		s.stopped = append(s.stopped, entry{c: c, startedAt: startedAt})
	}
	return nil
}

// Stopped lists the containers stopped so far.
func (s *Stopper) Stopped() []dockerx.Container {
	out := make([]dockerx.Container, 0, len(s.stopped))
	for _, e := range s.stopped {
		out = append(out, e.c)
	}
	return out
}

// Restart starts the stopped containers in ascending order of their original
// start time. Every failure is reported; the list is cleared afterwards.
// Callers pass a context that is not cancelled (context.WithoutCancel).
func (s *Stopper) Restart(ctx context.Context) []error {
	sort.SliceStable(s.stopped, func(i, j int) bool { return s.stopped[i].startedAt.Before(s.stopped[j].startedAt) })
	var errs []error
	for _, e := range s.stopped {
		fmt.Fprintf(s.out, "starting container %s\n", e.c.Name)
		if err := s.d.StartContainer(ctx, e.c.ID); err != nil {
			errs = append(errs, fmt.Errorf("start container %s: %w", e.c.Name, err))
		}
	}
	s.stopped = nil
	return errs
}
