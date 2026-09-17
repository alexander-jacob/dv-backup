package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

type section struct{ off, size int64 }

// Reader gives random access to the entries of an archive.
type Reader struct {
	f       *os.File
	m       *manifest.Manifest
	entries map[string]section
}

// Open scans the tar headers, reads manifest.yaml and validates that every
// manifest volume has an entry of the recorded size. Volume data is not read.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{f: f, entries: map[string]section{}}
	var manifestData []byte
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: not a dv-backup archive: %w", path, err)
		}
		// archive/tar reads headers straight from f, so the file offset now
		// points at the entry's first data byte. Next() skips data via Seek.
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			f.Close()
			return nil, err
		}
		switch {
		case hdr.Name == ManifestPath:
			if manifestData, err = io.ReadAll(tr); err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: read manifest: %w", path, err)
			}
		case hdr.Name == ReadmePath, isVolumeEntry(hdr.Name):
		default:
			f.Close()
			return nil, fmt.Errorf("%s: unexpected archive entry %q", path, hdr.Name)
		}
		r.entries[hdr.Name] = section{off: off, size: hdr.Size}
	}
	if manifestData == nil {
		f.Close()
		return nil, fmt.Errorf("%s: no manifest.yaml (incomplete backup?)", path)
	}
	m, err := manifest.Parse(manifestData)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, v := range m.Volumes {
		s, ok := r.entries[v.Archive.Path]
		if !ok {
			f.Close()
			return nil, fmt.Errorf("%s: manifest lists volume %s but entry %s is missing", path, v.Name, v.Archive.Path)
		}
		if s.size != v.Archive.SizeBytes {
			f.Close()
			return nil, fmt.Errorf("%s: entry %s has %d bytes, manifest says %d", path, v.Archive.Path, s.size, v.Archive.SizeBytes)
		}
	}
	r.m = m
	return r, nil
}

func isVolumeEntry(name string) bool {
	rest, ok := strings.CutPrefix(name, "volumes/")
	if !ok {
		return false
	}
	base, ok := strings.CutSuffix(rest, ".tar.zst")
	return ok && manifest.ValidName(base)
}

// Manifest returns the parsed manifest.
func (r *Reader) Manifest() *manifest.Manifest { return r.m }

func (r *Reader) compressed(name string) (*manifest.Volume, *io.SectionReader, error) {
	v, ok := r.m.Find(name)
	if !ok {
		return nil, nil, fmt.Errorf("volume %s is not in the archive", name)
	}
	s := r.entries[v.Archive.Path]
	return v, io.NewSectionReader(r.f, s.off, s.size), nil
}

// Verify recomputes the sha256 of the volume's compressed entry.
func (r *Reader) Verify(name string) error {
	v, sr, err := r.compressed(name)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, sr); err != nil {
		return fmt.Errorf("volume %s: read: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.Archive.SHA256 {
		return fmt.Errorf("volume %s: checksum mismatch (manifest %s, file %s)", name, v.Archive.SHA256, got)
	}
	return nil
}

// OpenVolume returns the decompressed tar stream of a volume.
func (r *Reader) OpenVolume(name string) (io.ReadCloser, error) {
	_, sr, err := r.compressed(name)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(sr)
	if err != nil {
		return nil, fmt.Errorf("volume %s: zstd: %w", name, err)
	}
	return dec.IOReadCloser(), nil
}

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }
