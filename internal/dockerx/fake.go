package dockerx

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Docker used by unit tests. Volume contents are modelled
// as an opaque byte slice standing in for the tar stream a helper would produce.
type Fake struct {
	mu         sync.Mutex
	Host       HostInfo
	Volumes    map[string]Volume
	Sizes      map[string]int64
	Data       map[string][]byte
	Containers map[string]*Container
	Started    map[string]time.Time
	Images     map[string]string // ref -> digest
	// Calls records every mutating or helper call, in order, e.g. "stop c1", "helper tar v".
	Calls []string
	// Failure injection.
	FailStop    map[string]error
	FailStart   map[string]error
	FailHelper  map[string]error // by "<entrypoint> <volume>", e.g. "tar app_db": RunHelper returns this error
	HelperExit  map[string]int   // by volume: backup tar exit code
	FailInfo    error
	FailInspect map[string]error
}

// NewFake returns an empty fake with a known host and the default image present.
func NewFake() *Fake {
	return &Fake{
		Host:       HostInfo{Name: "fakehost", ServerVersion: "29.8.1"},
		Volumes:    map[string]Volume{},
		Sizes:      map[string]int64{},
		Data:       map[string][]byte{},
		Containers: map[string]*Container{},
		Started:    map[string]time.Time{},
		Images:     map[string]string{DefaultImage: DefaultImage + "@sha256:fake"},
		FailStop:   map[string]error{}, FailStart: map[string]error{}, FailHelper: map[string]error{},
		HelperExit: map[string]int{}, FailInspect: map[string]error{},
	}
}

// AddVolume registers a volume with optional data.
func (f *Fake) AddVolume(v Volume, data []byte) {
	f.Volumes[v.Name] = v
	if data != nil {
		f.Data[v.Name] = data
	}
}

// AddContainer registers a container; StartedAt is derived from insertion order unless set via Started.
func (f *Fake) AddContainer(c Container) {
	cc := c
	f.Containers[c.ID] = &cc
	if _, ok := f.Started[c.ID]; !ok {
		f.Started[c.ID] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(f.Containers)) * time.Minute)
	}
}

func (f *Fake) record(format string, a ...any) { f.Calls = append(f.Calls, fmt.Sprintf(format, a...)) }

// Info implements Docker.
func (f *Fake) Info(context.Context) (HostInfo, error) { return f.Host, f.FailInfo }

// ListVolumes implements Docker (sorted by name).
func (f *Fake) ListVolumes(context.Context) ([]Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Volume, 0, len(f.Volumes))
	for _, v := range f.Volumes {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// VolumeSizes implements Docker.
func (f *Fake) VolumeSizes(context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for k, v := range f.Sizes {
		out[k] = v
	}
	return out, nil
}

// InspectVolume implements Docker.
func (f *Fake) InspectVolume(_ context.Context, name string) (Volume, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.FailInspect[name]; err != nil {
		return Volume{}, false, err
	}
	v, ok := f.Volumes[name]
	return v, ok, nil
}

// CreateVolume implements Docker.
func (f *Fake) CreateVolume(_ context.Context, v Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-volume %s", v.Name)
	if _, ok := f.Volumes[v.Name]; ok {
		return nil // Docker's create is idempotent
	}
	f.Volumes[v.Name] = v
	return nil
}

// ListContainers implements Docker (sorted by ID).
func (f *Fake) ListContainers(context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Container, 0, len(f.Containers))
	for _, c := range f.Containers {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// StartedAt implements Docker.
func (f *Fake) StartedAt(_ context.Context, id string) (time.Time, error) {
	if _, ok := f.Containers[id]; !ok {
		return time.Time{}, fmt.Errorf("no such container: %s", id)
	}
	return f.Started[id], nil
}

// StopContainer implements Docker.
func (f *Fake) StopContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop %s", id)
	if err := f.FailStop[id]; err != nil {
		return err
	}
	c, ok := f.Containers[id]
	if !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	c.State = StateExited
	return nil
}

// StartContainer implements Docker.
func (f *Fake) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start %s", id)
	if err := f.FailStart[id]; err != nil {
		return err
	}
	c, ok := f.Containers[id]
	if !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	c.State = StateRunning
	return nil
}

// EnsureImage implements Docker.
func (f *Fake) EnsureImage(_ context.Context, ref string) (string, error) {
	if d, ok := f.Images[ref]; ok {
		return d, nil
	}
	return "", fmt.Errorf("pull %s: not found (fake)", ref)
}

// RunHelper implements Docker with tar/find semantics over Data.
func (f *Fake) RunHelper(ctx context.Context, h Helper) (HelperResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("helper %s %s", h.Entrypoint, h.Volume)
	if err := ctx.Err(); err != nil {
		return HelperResult{}, err
	}
	if err := f.FailHelper[h.Entrypoint+" "+h.Volume]; err != nil {
		return HelperResult{}, err
	}
	if _, ok := f.Volumes[h.Volume]; !ok {
		return HelperResult{}, fmt.Errorf("create helper container: no such volume: %s", h.Volume)
	}
	stdout := h.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	switch h.Entrypoint {
	case "tar":
		if h.Stdin != nil {
			b, err := io.ReadAll(h.Stdin)
			if err != nil {
				return HelperResult{}, err
			}
			f.Data[h.Volume] = b
			return HelperResult{}, nil
		}
		if _, err := stdout.Write(f.Data[h.Volume]); err != nil {
			return HelperResult{}, err
		}
		if code := f.HelperExit[h.Volume]; code != 0 {
			return HelperResult{ExitCode: code, Stderr: "tar: ./x: file changed as we read it"}, nil
		}
		return HelperResult{}, nil
	case "find":
		if strings.Contains(strings.Join(h.Args, " "), "-delete") {
			delete(f.Data, h.Volume)
			return HelperResult{}, nil
		}
		if len(f.Data[h.Volume]) > 0 {
			_, _ = io.WriteString(stdout, DataDir+"/x\n")
		}
		return HelperResult{}, nil
	}
	return HelperResult{}, fmt.Errorf("fake: unsupported entrypoint %q", h.Entrypoint)
}

// Close implements Docker.
func (f *Fake) Close() error { return nil }

var _ Docker = (*Fake)(nil)
