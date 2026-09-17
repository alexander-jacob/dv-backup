//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/alexander-jacob/dv-backup/internal/archive"
	"github.com/alexander-jacob/dv-backup/internal/dockerx"
)

func TestRoundTripFidelity(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(map[string]string{"com.docker.compose.project": "dvtest", "com.docker.compose.volume": "data"})
	h.populate(vol)
	before := h.listing(vol)
	if !strings.Contains(before, "sparse.img") || !strings.Contains(before, "999:999") {
		t.Fatalf("fixture incomplete:\n%s", before)
	}

	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := h.raw.VolumeRemove(h.ctx, vol, client.VolumeRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, out, err := h.restore(res.Path, false, vol); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	after := h.listing(vol)
	if before != after {
		t.Fatalf("listing differs\n--- before\n%s\n--- after\n%s", before, after)
	}
	v, ok, err := h.d.InspectVolume(h.ctx, vol)
	if err != nil || !ok || v.Labels["com.docker.compose.project"] != "dvtest" || v.Labels["com.docker.compose.volume"] != "data" {
		t.Fatalf("labels not restored: %+v ok=%v err=%v", v, ok, err)
	}
	// Sparse file still sparse: du reports far less than 100 MB. Scope the
	// scan to the "disk usage" section only: the md5sum section above it also
	// has a line ending in "sparse.img" (the hash), whose first field would
	// otherwise be mistaken for a huge block count.
	duSection := after[strings.Index(after, "--- disk usage"):]
	for _, line := range strings.Split(duSection, "\n") {
		if strings.HasSuffix(line, "sparse.img") {
			kb := strings.Fields(line)[0]
			if len(kb) > 4 { // more than 9999 KiB means it was expanded
				t.Fatalf("sparse file expanded: %s", line)
			}
		}
	}
}

// Xattrs travel as PAX records: build a tar with them in Go, push it through the
// restore helper, back the volume up and check the records survived.
func TestXattrsRoundTrip(t *testing.T) {
	h := newHarness(t)
	vol := h.volume(nil)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	capNetRaw := "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" // VFS_CAP_REVISION_2, cap_net_raw=ep
	_ = tw.WriteHeader(&tar.Header{Name: "./x", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		PAXRecords: map[string]string{"SCHILY.xattr.user.dvtest": "hello", "SCHILY.xattr.security.capability": capNetRaw}})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if r, err := h.d.RunHelper(h.ctx, dockerx.RestoreHelper(dockerx.DefaultImage, vol, &buf)); err != nil || r.ExitCode != 0 {
		t.Fatalf("restore helper: %+v %v", r, err)
	}
	res, out, err := h.backup(t.TempDir(), vol)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ar, err := archive.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	rc, err := ar.OpenVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatal("./x not found in backup stream")
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != "./x" {
			continue
		}
		if hdr.PAXRecords["SCHILY.xattr.user.dvtest"] != "hello" {
			t.Fatalf("user xattr lost: %v", hdr.PAXRecords)
		}
		if hdr.PAXRecords["SCHILY.xattr.security.capability"] != capNetRaw {
			t.Fatalf("security.capability lost (see spec 10.2; consider --xattrs-include='*'): %q", hdr.PAXRecords["SCHILY.xattr.security.capability"])
		}
		return
	}
}
