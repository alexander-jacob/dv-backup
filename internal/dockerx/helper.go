package dockerx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		t.buf.Next(t.buf.Len() - t.max)
	}
	return len(p), nil
}

// RunHelper runs one helper container to completion. See spec §11.2 for the
// ordering rules: attach and wait are set up before start so nothing is lost;
// the container is removed explicitly with a non-cancelled context.
func (d *Client) RunHelper(ctx context.Context, h Helper) (HelperResult, error) {
	cfg := &container.Config{
		Image:        h.Image,
		Entrypoint:   []string{h.Entrypoint},
		Cmd:          h.Args,
		User:         "0:0",
		Tty:          false,
		AttachStdout: true,
		AttachStderr: true,
		Labels:       map[string]string{LabelHelper: "true"},
	}
	if h.Stdin != nil {
		cfg.OpenStdin, cfg.StdinOnce, cfg.AttachStdin = true, true, true
	}
	host := &container.HostConfig{
		NetworkMode: "none",
		Mounts: []mount.Mount{{
			Type:          mount.TypeVolume,
			Source:        h.Volume,
			Target:        DataDir,
			ReadOnly:      h.ReadOnly,
			VolumeOptions: &mount.VolumeOptions{NoCopy: true},
		}},
	}
	created, err := d.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: host, Name: "dv-backup-helper-" + randomHex(6),
	})
	if err != nil {
		return HelperResult{}, fmt.Errorf("create helper container: %w", err)
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = d.c.ContainerRemove(rmCtx, created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	attach, err := d.c.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true, Stdin: h.Stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		return HelperResult{}, fmt.Errorf("attach to helper container: %w", err)
	}
	defer attach.Close()

	// The hijacked connection is not itself tied to ctx (only the initial
	// attach request is): cancelling ctx must still unblock a stdin copy
	// that is stuck writing (e.g. Ctrl-C during a multi-GB restore) or a
	// StdCopy read that is stuck waiting for more output. Closing the
	// connection makes both fail promptly instead of hanging.
	stopOnCancel := context.AfterFunc(ctx, attach.Close)
	defer stopOnCancel()

	wait := d.c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	if _, err := d.c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return HelperResult{}, fmt.Errorf("start helper container: %w", err)
	}

	stderr := &tailBuffer{max: 64 * 1024}
	stdout := h.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, attach.Reader)
		outDone <- err
	}()

	// The copy runs in its own goroutine so a cancelled ctx is not ignored
	// while it is in flight: io.Copy itself has no notion of ctx, and
	// h.Stdin may be a reader that blocks indefinitely (e.g. a stalled
	// pipeline). We wait for it when it finishes on its own, but on
	// cancellation we still must not return until the goroutine has
	// actually stopped touching h.Stdin: our only caller (restore) defers
	// Close on the same reader right after RunHelper returns, and a
	// goroutine still inside Read would race that Close.
	//
	// Contract on Helper.Stdin: a Read in progress must always return (not
	// block forever) once its data is available or its source is
	// exhausted - it does not need to observe ctx itself. On cancellation
	// we close attach, which unblocks a write that is stuck on a full
	// connection buffer; the in-flight Read then completes on its own (per
	// the contract), the next Write fails because attach is closed, and
	// the goroutine exits, so waiting for it here cannot hang. Callers that
	// read from files (the only caller today) satisfy this.
	var sendErr error
	if h.Stdin != nil {
		sendDone := make(chan error, 1)
		go func() {
			_, err := io.Copy(attach.Conn, h.Stdin)
			if cwErr := attach.CloseWrite(); err == nil {
				err = cwErr
			}
			sendDone <- err
		}()
		select {
		case sendErr = <-sendDone: // tar may have exited early; report after we know its exit code
		case <-ctx.Done():
			attach.Close()
			<-sendDone
			sendErr = ctx.Err()
		}
	}

	// awaitOutput closes the attach connection - unblocking a StdCopy that is
	// still reading, e.g. after ctx cancellation - and waits for the copy
	// goroutine to finish. It must run before RunHelper returns on every path
	// below, so the goroutine can never outlive this call and race the
	// caller's use of h.Stdout after we hand back control. It is idempotent
	// (guarded by outDrained) so it is safe to call from more than one path.
	var outErr error
	outDrained := false
	awaitOutput := func() error {
		if !outDrained {
			outDrained = true
			attach.Close()
			outErr = <-outDone
		}
		return outErr
	}

	// Either the output stream ends (container exited, or ctx cancellation
	// closed the connection) or the container's wait resolves first; either
	// way we still need the other result before returning.
	select {
	case outErr = <-outDone:
		outDrained = true
	case res := <-wait.Result:
		return d.finish(res, stderr, sendErr, awaitOutput())
	case err := <-wait.Error:
		_ = awaitOutput()
		return HelperResult{}, fmt.Errorf("wait for helper container: %w", err)
	}
	if outErr != nil && !errors.Is(outErr, io.EOF) {
		return HelperResult{}, fmt.Errorf("read helper output: %w", outErr)
	}
	select {
	case res := <-wait.Result:
		return d.finish(res, stderr, sendErr, nil)
	case err := <-wait.Error:
		_ = awaitOutput()
		return HelperResult{}, fmt.Errorf("wait for helper container: %w", err)
	}
}

func (d *Client) finish(res container.WaitResponse, stderr *tailBuffer, sendErr error, outErr error) (HelperResult, error) {
	if outErr != nil && !errors.Is(outErr, io.EOF) {
		return HelperResult{}, fmt.Errorf("read helper output: %w", outErr)
	}
	if res.Error != nil {
		return HelperResult{}, fmt.Errorf("helper container: %s", res.Error.Message)
	}
	r := HelperResult{ExitCode: int(res.StatusCode), Stderr: stderr.buf.String()}
	if sendErr != nil && r.ExitCode == 0 {
		return r, fmt.Errorf("send data to helper container: %w", sendErr)
	}
	return r, nil
}
