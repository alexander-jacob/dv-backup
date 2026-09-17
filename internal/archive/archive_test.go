package archive

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

var testTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// fakeTarStream returns bytes that stand in for a helper's tar output; the
// writer treats the stream as opaque.
func fakeTarStream(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%7)
	}
	return b
}

func newManifest(entries ...manifest.Volume) *manifest.Manifest {
	return &manifest.Manifest{FormatVersion: manifest.FormatVersion, Tool: "dv-backup test", CreatedAt: testTime,
		Host: "vm-app-01", DockerVersion: "29.8.1", HelperImage: "debian:13-slim@sha256:x", Volumes: entries}
}

func TestFileName(t *testing.T) {
	got := FileName("vm app/01", testTime)
	if got != "dv-backup-vm-app-01-20260917T120000Z.tar" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterProducesReadableTar(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "vm-app-01", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dv-backup-vm-app-01-20260917T120000Z.tar.partial")); err != nil {
		t.Fatalf("partial file missing: %v", err)
	}
	data := fakeTarStream(1, 100_000)
	entry, n, err := w.AddVolume("vol_a", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) || entry.Path != "volumes/vol_a.tar.zst" || entry.SizeBytes <= 0 {
		t.Fatalf("entry = %+v, n = %d", entry, n)
	}
	m := newManifest(manifest.Volume{Name: "vol_a", Driver: "local", SizeBytes: n, Archive: entry, Consistent: true})
	if err := w.Finish(m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path()); err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	info, _ := os.Stat(w.Path())
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".dv-backup-*"))
	partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	if len(leftovers) != 0 || len(partials) != 0 {
		t.Fatalf("temp files left: %v %v", leftovers, partials)
	}

	// Read back with archive/tar and check order, checksum and content.
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		body, _ := io.ReadAll(tr)
		switch hdr.Name {
		case "volumes/vol_a.tar.zst":
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != entry.SHA256 || int64(len(body)) != entry.SizeBytes {
				t.Fatal("checksum or size mismatch")
			}
			dec, _ := zstd.NewReader(bytes.NewReader(body))
			plain, _ := io.ReadAll(dec)
			dec.Close()
			if !bytes.Equal(plain, data) {
				t.Fatal("decompressed data differs")
			}
		case ManifestPath:
			if _, err := manifest.Parse(body); err != nil {
				t.Fatalf("manifest in archive invalid: %v", err)
			}
		case ReadmePath:
			if !strings.Contains(string(body), "vol_a") {
				t.Fatal("README does not mention volume")
			}
		}
	}
	if strings.Join(names, ",") != "volumes/vol_a.tar.zst,manifest.yaml,README.md" {
		t.Fatalf("entry order %v", names)
	}
}

func TestWriterRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, FileName("h", testTime))
	if err := os.WriteFile(final, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, "h", testTime); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("expected refusal, got %v", err)
	}
	os.Remove(final)
	if err := os.WriteFile(final+".partial", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, "h", testTime); err == nil {
		t.Fatal("expected refusal for existing .partial")
	}
}

func TestWriterAbortRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "h", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AddVolume("v", bytes.NewReader(fakeTarStream(2, 10))); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("directory not clean: %v", files)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestWriterAddVolumeErrorCleansTemp(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "h", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AddVolume("v", failingReader{}); err == nil {
		t.Fatal("expected error")
	}
	_ = w.Abort()
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("directory not clean: %v", files)
	}
}
