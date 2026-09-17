//go:build integration

package integration

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSIGINTDuringBackupRestartsAndCleans(t *testing.T) {
	h := newHarness(t)
	bin := filepath.Join(t.TempDir(), "dv-backup")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/dv-backup")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	vol := h.volume(nil)
	h.sh(vol, "head -c 600M /dev/urandom > /data/big.bin") // incompressible, takes a few seconds to stream
	id := h.container(vol, true, nil)

	outDir := t.TempDir()
	cmd := exec.Command(bin, "backup", "-o", outDir, vol)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "backing up volume") {
			break
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	for sc.Scan() {
	}
	err = cmd.Wait()
	if err == nil {
		t.Fatal("backup should have been interrupted (if it finished, increase the fixture size)")
	}
	if h.state(id) != "running" {
		t.Fatalf("container not restarted after SIGINT: %s", h.state(id))
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Fatalf("output dir not clean: %v", entries)
	}
}
