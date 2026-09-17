//go:build integration

package integration

import (
	"testing"

	"github.com/moby/moby/client"
)

// TestLargeIncompressibleRoundTrip backs up and restores a volume holding
// 300 MB of random data and compares checksums. A large, incompressible
// stream is what exposes the helper closing its attach connection while the
// daemon still has output buffered after reporting the container's exit.
func TestLargeIncompressibleRoundTrip(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	const sums = "cd /data && sha256sum big.bin && stat -c '%s %u:%g %a' big.bin"
	h.sh(vol, "head -c 300M /dev/urandom > /data/big.bin")
	before := h.sh(vol, sums)

	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	if _, err := h.raw.VolumeRemove(h.ctx, vol, client.VolumeRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	// The volume's cleanup is already registered; restore recreates it.
	if out, err := h.restore(res.Path, false, vol); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if after := h.sh(vol, sums); after != before {
		t.Fatalf("checksum mismatch after round trip:\nbefore %s\nafter  %s", before, after)
	}
}
