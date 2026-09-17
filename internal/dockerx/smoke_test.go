//go:build integration

package dockerx

import (
	"context"
	"testing"
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
