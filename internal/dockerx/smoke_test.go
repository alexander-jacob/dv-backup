//go:build integration

package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"

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
	cs, _ := d.ListContainers(ctx)
	for _, c := range cs {
		if c.IsHelper() && strings.Contains(c.Name, "dv-backup-helper-") {
			t.Fatalf("stray helper %s", c.Name)
		}
	}
}
