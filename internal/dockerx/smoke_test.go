//go:build integration

package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

func TestClientSmoke(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	info, err := d.Info(ctx)
	if err != nil || info.Name == "" || info.ServerVersion == "" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if _, err := d.ListVolumes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListContainers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.InspectVolume(ctx, "dv-backup-test-does-not-exist"); err != nil || ok {
		t.Fatalf("missing volume: ok=%v err=%v", ok, err)
	}
	digest, err := d.EnsureImage(ctx, DefaultImage)
	if err != nil || digest == "" {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	t.Log("host", info.Name, "image", digest)
}

func TestHelperRoundTrip(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	name := "dv-backup-test-helper-" + randomHex(4)
	if err := d.CreateVolume(ctx, Volume{Name: name, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.c.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	var out bytes.Buffer
	res, err := d.RunHelper(ctx, EmptyCheckHelper(DefaultImage, name, &out))
	if err != nil || res.ExitCode != 0 || out.Len() != 0 {
		t.Fatalf("fresh volume not empty: res=%+v err=%v out=%q", res, err, out.String())
	}

	// Build a tar stream in Go and restore it through the helper.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "./hello.txt", Mode: 0o640, Uid: 1234, Gid: 1234, Size: 5, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	res, err = d.RunHelper(ctx, RestoreHelper(DefaultImage, name, &tarBuf))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("restore: res=%+v err=%v", res, err)
	}

	out.Reset()
	res, err = d.RunHelper(ctx, BackupHelper(DefaultImage, name, &out))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("backup: res=%+v err=%v", res, err)
	}
	tr := tar.NewReader(&out)
	found := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == "./hello.txt" {
			found = true
			if hdr.Uid != 1234 || hdr.Mode&0o777 != 0o640 {
				t.Fatalf("metadata lost: %+v", hdr)
			}
		}
	}
	if !found {
		t.Fatal("hello.txt missing from backup stream")
	}

	// Exit code and stderr surface for a failing command.
	res, err = d.RunHelper(ctx, Helper{Image: DefaultImage, Volume: name, Entrypoint: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err != nil || res.ExitCode != 3 || !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("exit code path: res=%+v err=%v", res, err)
	}

	// No helper containers left behind.
	assertNoStrayHelpers(t, d)
}

// TestHelperLogDriverNone proves that helper containers are created with the
// "none" log driver, so the daemon does not also copy the (possibly
// multi-GB) tar stream into the container's json-file log, and that attach
// still delivers stdout and stderr with that driver.
func TestHelperLogDriverNone(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	name := "dv-backup-test-helper-log-" + randomHex(4)
	if err := d.CreateVolume(ctx, Volume{Name: name, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.c.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	type result struct {
		res HelperResult
		err error
	}
	var out bytes.Buffer
	done := make(chan result, 1)
	go func() {
		res, err := d.RunHelper(ctx, Helper{Image: DefaultImage, Volume: name, Entrypoint: "sh",
			Args: []string{"-c", "echo to-stdout; echo to-stderr >&2; sleep 3"}, Stdout: &out})
		done <- result{res, err}
	}()

	// Find the helper while it runs (it mounts our uniquely named test
	// volume) and inspect its log configuration.
	logType := ""
	deadline := time.Now().Add(20 * time.Second)
	for logType == "" && time.Now().Before(deadline) {
		cs, err := d.ListContainers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.IsHelper() && c.UsesVolume(name) {
				ins, err := d.c.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
				if err == nil && ins.Container.HostConfig != nil {
					logType = ins.Container.HostConfig.LogConfig.Type
				}
			}
		}
		if logType == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	r := <-done
	if logType != "none" {
		t.Fatalf("helper log driver = %q, want \"none\"", logType)
	}
	if r.err != nil || r.res.ExitCode != 0 || out.String() != "to-stdout\n" || !strings.Contains(r.res.Stderr, "to-stderr") {
		t.Fatalf("attach output with log driver none: res=%+v err=%v stdout=%q", r.res, r.err, out.String())
	}
	assertNoStrayHelpers(t, d)
}

// countingSlowWriter counts bytes and sleeps per write, so the helper
// container exits (and wait.Result fires) while output is still buffered in
// the daemon and the attach connection.
type countingSlowWriter struct {
	n     int
	delay time.Duration
}

func (w *countingSlowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.n += len(p)
	return len(p), nil
}

// TestHelperDrainsOutputAfterExit proves RunHelper reads all buffered output
// after the container has exited instead of closing the attach connection
// as soon as wait.Result arrives.
func TestHelperDrainsOutputAfterExit(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	name := "dv-backup-test-helper-drain-" + randomHex(4)
	if err := d.CreateVolume(ctx, Volume{Name: name, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.c.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	const size = 4 << 20
	w := &countingSlowWriter{delay: 2 * time.Millisecond}
	res, err := d.RunHelper(ctx, Helper{Image: DefaultImage, Volume: name, Entrypoint: "head",
		Args: []string{"-c", strconv.Itoa(size), "/dev/urandom"}, Stdout: w})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v (read %d of %d bytes)", res, err, w.n, size)
	}
	if w.n != size {
		t.Fatalf("read %d bytes, want %d", w.n, size)
	}
	assertNoStrayHelpers(t, d)
}

// assertNoStrayHelpers fails the test if any dv-backup helper container is
// still around. It uses its own context since the caller's ctx may already
// be cancelled (e.g. in the cancellation tests below).
func assertNoStrayHelpers(t *testing.T, d *Client) {
	t.Helper()
	cs, err := d.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	for _, c := range cs {
		if c.IsHelper() && strings.Contains(c.Name, "dv-backup-helper-") {
			t.Fatalf("stray helper %s", c.Name)
		}
	}
}

// slowWriter drains data but sleeps briefly per write, so a large enough
// stream stays in flight long enough for a cancellation to land mid-copy.
// Unlike an unread pipe, it never blocks indefinitely, so a bug that fails
// to react to cancellation shows up as a slow test rather than a hang.
type slowWriter struct{ delay time.Duration }

func (w slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return len(p), nil
}

// closeRaceReader implements io.Reader with a deliberately slow but always-
// returning Read (never EOF; sleeps readDelay then returns data), so it is
// still in flight when ctx is cancelled shortly after RunHelper starts
// sending it as stdin. This proves two things about RunHelper's send
// goroutine:
//
//  1. Timing: RunHelper must not return before the in-flight Read finishes
//     (i.e. it must join the send goroutine, not just stop waiting on it),
//     so the test asserts elapsed >= readDelay.
//  2. Memory safety: Read and Close deliberately touch the same field, n,
//     with no synchronization between them, mirroring the real restore
//     caller (Task 13), which defers Close on the same reader right after
//     RunHelper returns. If the send goroutine were still inside Read at
//     that point, `go test -race` would report a data race between it and
//     the test's Close call.
//
// A correct implementation joins the goroutine before returning, so
// RunHelper cannot return before readDelay has elapsed and Close always
// happens strictly after the last Read.
type closeRaceReader struct {
	n         int
	readDelay time.Duration
}

func (r *closeRaceReader) Read(p []byte) (int, error) {
	time.Sleep(r.readDelay)
	r.n++
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func (r *closeRaceReader) Close() error {
	r.n++
	return nil
}

// TestHelperRestoreCancellation proves that cancelling ctx while RunHelper is
// sending stdin for a restore makes RunHelper return promptly with an error,
// does not leave the helper container behind, and does not leave the send
// goroutine still reading from h.Stdin after it returns (it must have
// observed the closed connection and exited, per the Helper.Stdin contract
// documented in helper.go).
func TestHelperRestoreCancellation(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	name := "dv-backup-test-helper-cancel-restore-" + randomHex(4)
	if err := d.CreateVolume(context.Background(), Volume{Name: name, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.c.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	const readDelay = 1500 * time.Millisecond
	r := &closeRaceReader{readDelay: readDelay}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond) // well before readDelay elapses
		cancel()
	}()

	start := time.Now()
	res, err := d.RunHelper(ctx, RestoreHelper(DefaultImage, name, r))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected an error after cancellation, got res=%+v", res)
	}
	if elapsed < readDelay {
		t.Fatalf("RunHelper returned after %v, before the in-flight Read (delay %v) could have finished: send goroutine was not joined", elapsed, readDelay)
	}
	if elapsed > readDelay+9*time.Second {
		t.Fatalf("RunHelper took %v to return after ctx cancellation", elapsed)
	}
	assertNoStrayHelpers(t, d)

	// Mirrors the real restore caller's defer rc.Close() right after
	// RunHelper returns. If the send goroutine outlived RunHelper (still
	// inside Read), this races with it on r.n and `go test -race` fails
	// the test; see closeRaceReader's doc comment.
	_ = r.Close()
}

// TestHelperBackupCancellation proves that cancelling ctx while RunHelper is
// streaming a backup makes RunHelper return promptly with an error, and does
// not leave the helper container behind.
func TestHelperBackupCancellation(t *testing.T) {
	d, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	name := "dv-backup-test-helper-cancel-backup-" + randomHex(4)
	if err := d.CreateVolume(context.Background(), Volume{Name: name, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.c.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	// Seed the volume with enough data that streaming it back out takes
	// noticeably longer than the cancellation delay below.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	payload := bytes.Repeat([]byte("x"), 4<<20) // 4 MiB
	_ = tw.WriteHeader(&tar.Header{Name: "./big.bin", Mode: 0o640, Size: int64(len(payload)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	if res, err := d.RunHelper(context.Background(), RestoreHelper(DefaultImage, name, &tarBuf)); err != nil || res.ExitCode != 0 {
		t.Fatalf("seed restore: res=%+v err=%v", res, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := d.RunHelper(ctx, BackupHelper(DefaultImage, name, slowWriter{delay: 20 * time.Millisecond}))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected an error after cancellation, got res=%+v", res)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("RunHelper took %v to return after ctx cancellation", elapsed)
	}
	assertNoStrayHelpers(t, d)
}
