package dockerx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestFakeHelperSemantics(t *testing.T) {
	f := NewFake()
	f.Volumes["v"] = Volume{Name: "v", Driver: "local"}
	ctx := context.Background()

	var out bytes.Buffer
	res, err := f.RunHelper(ctx, EmptyCheckHelper(DefaultImage, "v", &out))
	if err != nil || res.ExitCode != 0 || out.Len() != 0 {
		t.Fatalf("empty volume: res=%+v err=%v out=%q", res, err, out.String())
	}

	if _, err := f.RunHelper(ctx, RestoreHelper(DefaultImage, "v", strings.NewReader("TARDATA"))); err != nil {
		t.Fatal(err)
	}
	if string(f.Data["v"]) != "TARDATA" {
		t.Fatalf("restore did not store data: %q", f.Data["v"])
	}

	out.Reset()
	if _, err := f.RunHelper(ctx, EmptyCheckHelper(DefaultImage, "v", &out)); err != nil || out.Len() == 0 {
		t.Fatalf("non-empty volume should print a path")
	}

	out.Reset()
	if _, err := f.RunHelper(ctx, BackupHelper(DefaultImage, "v", &out)); err != nil || out.String() != "TARDATA" {
		t.Fatalf("backup output %q err %v", out.String(), err)
	}

	if _, err := f.RunHelper(ctx, ClearHelper(DefaultImage, "v")); err != nil || len(f.Data["v"]) != 0 {
		t.Fatal("clear did not empty the volume")
	}
	if _, err := f.RunHelper(ctx, BackupHelper(DefaultImage, "missing", &out)); err == nil {
		t.Fatal("helper on missing volume must fail")
	}
	if len(f.Calls) == 0 || !strings.HasPrefix(f.Calls[0], "helper find v") {
		t.Fatalf("calls not recorded: %v", f.Calls)
	}
}

func TestFakeContainers(t *testing.T) {
	f := NewFake()
	f.AddContainer(Container{ID: "c1", Name: "app", State: StateRunning, Mounts: []Mount{{Type: "volume", Name: "v"}}})
	ctx := context.Background()
	if err := f.StopContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	cs, _ := f.ListContainers(ctx)
	if cs[0].State != StateExited {
		t.Fatalf("state after stop = %s", cs[0].State)
	}
	if err := f.StartContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	cs, _ = f.ListContainers(ctx)
	if cs[0].State != StateRunning {
		t.Fatalf("state after start = %s", cs[0].State)
	}
	if err := f.StopContainer(ctx, "nope"); err == nil {
		t.Fatal("unknown container must fail")
	}
}
