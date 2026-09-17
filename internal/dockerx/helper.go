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

	var sendErr error
	if h.Stdin != nil {
		if _, err := io.Copy(attach.Conn, h.Stdin); err != nil {
			sendErr = err // tar may have exited early; report after we know its exit code
		}
		_ = attach.CloseWrite()
	}

	// Either the output stream ends (container exited) or reading it fails
	// (e.g. our consumer stopped); in the latter case return so the deferred
	// force-remove kills the helper instead of letting it block on stdout.
	var outErr error
	outClosed := false
	select {
	case outErr = <-outDone:
		outClosed = true
	case res := <-wait.Result:
		return d.finish(res, stderr, sendErr, outDone)
	case err := <-wait.Error:
		return HelperResult{}, fmt.Errorf("wait for helper container: %w", err)
	}
	if outClosed && outErr != nil && !errors.Is(outErr, io.EOF) {
		return HelperResult{}, fmt.Errorf("read helper output: %w", outErr)
	}
	select {
	case res := <-wait.Result:
		return d.finish(res, stderr, sendErr, nil)
	case err := <-wait.Error:
		return HelperResult{}, fmt.Errorf("wait for helper container: %w", err)
	}
}

func (d *Client) finish(res container.WaitResponse, stderr *tailBuffer, sendErr error, outDone <-chan error) (HelperResult, error) {
	if outDone != nil {
		if err := <-outDone; err != nil && !errors.Is(err, io.EOF) {
			return HelperResult{}, fmt.Errorf("read helper output: %w", err)
		}
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
