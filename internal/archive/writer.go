// Package archive reads and writes the outer dv-backup tar file.
package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/alexander-jacob/dv-backup/internal/manifest"
)

// Entry names inside the outer tar.
const (
	ManifestPath = "manifest.yaml"
	ReadmePath   = "README.md"
)

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// FileName returns the archive file name for a host and time (UTC).
func FileName(host string, t time.Time) string {
	return "dv-backup-" + unsafeChars.ReplaceAllString(host, "-") + "-" + t.UTC().Format("20060102T150405Z") + ".tar"
}

// Writer builds an archive in <dir>/<name>.tar.partial and renames it on Finish.
type Writer struct {
	dir     string
	final   string
	partial string
	f       *os.File
	tw      *tar.Writer
	done    bool
}

// NewWriter creates the .partial file. It refuses to touch an existing .tar or .partial.
func NewWriter(dir, host string, now time.Time) (*Writer, error) {
	final := filepath.Join(dir, FileName(host, now))
	partial := final + ".partial"
	if _, err := os.Stat(final); err == nil {
		return nil, fmt.Errorf("%s already exists; refusing to overwrite", final)
	}
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s already exists (unfinished backup?); remove it first", partial)
		}
		return nil, fmt.Errorf("create %s: %w", partial, err)
	}
	return &Writer{dir: dir, final: final, partial: partial, f: f, tw: tar.NewWriter(f)}, nil
}

// Path returns the final archive path.
func (w *Writer) Path() string { return w.final }

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// AddVolume compresses src with zstd into a temp file (hashing as it goes) and
// appends it to the outer tar. It returns the manifest entry and the number of
// uncompressed bytes read from src.
func (w *Writer) AddVolume(name string, src io.Reader) (manifest.Entry, int64, error) {
	tmp, err := os.CreateTemp(w.dir, ".dv-backup-"+name+"-*.tmp")
	if err != nil {
		return manifest.Entry{}, 0, fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return manifest.Entry{}, 0, err
	}
	hash := sha256.New()
	counter := &countingWriter{w: io.MultiWriter(tmp, hash)}
	enc, err := zstd.NewWriter(counter, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return manifest.Entry{}, 0, err
	}
	uncompressed, err := io.Copy(enc, src)
	if err != nil {
		enc.Close()
		return manifest.Entry{}, 0, fmt.Errorf("volume %s: %w", name, err)
	}
	if err := enc.Close(); err != nil {
		return manifest.Entry{}, 0, fmt.Errorf("volume %s: compress: %w", name, err)
	}
	entry := manifest.Entry{Path: manifest.VolumePath(name), SizeBytes: counter.n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return manifest.Entry{}, 0, err
	}
	if err := w.addEntry(entry.Path, entry.SizeBytes, tmp); err != nil {
		return manifest.Entry{}, 0, fmt.Errorf("volume %s: write archive: %w", name, err)
	}
	return entry, uncompressed, nil
}

func (w *Writer) addEntry(name string, size int64, r io.Reader) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now().UTC(), Typeflag: tar.TypeReg}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := io.Copy(w.tw, r)
	return err
}

// Finish writes manifest.yaml and README.md, closes the file and renames it to its final name.
func (w *Writer) Finish(m *manifest.Manifest) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	if err := w.addEntry(ManifestPath, int64(len(data)), bytesReader(data)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	readme := []byte(manifest.RenderREADME(m))
	if err := w.addEntry(ReadmePath, int64(len(readme)), bytesReader(readme)); err != nil {
		return fmt.Errorf("write README: %w", err)
	}
	if err := w.tw.Close(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.partial, w.final); err != nil {
		return err
	}
	w.done = true
	return nil
}

// Abort removes the .partial file. Safe to call after Finish (no-op).
func (w *Writer) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	_ = w.f.Close()
	if err := os.Remove(w.partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func bytesReader(b []byte) io.Reader { return &byteSlice{b: b} }

type byteSlice struct{ b []byte }

func (s *byteSlice) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}
