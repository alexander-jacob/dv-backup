package dockerx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// Client implements Docker on top of the moby client.
type Client struct {
	c *client.Client
}

// Connect creates a client from the environment (DOCKER_HOST etc.).
// API version negotiation happens on the first request.
func Connect() (*Client, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{c: c}, nil
}

// Close implements Docker.
func (d *Client) Close() error { return d.c.Close() }

// Info implements Docker.
func (d *Client) Info(ctx context.Context) (HostInfo, error) {
	info, err := d.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return HostInfo{}, fmt.Errorf("cannot reach Docker (is the daemon running and DOCKER_HOST correct?): %w", err)
	}
	ver, err := d.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return HostInfo{}, fmt.Errorf("docker version: %w", err)
	}
	return HostInfo{Name: info.Info.Name, ServerVersion: ver.Version}, nil
}

func fromVolume(v volume.Volume) Volume {
	return Volume{Name: v.Name, Driver: v.Driver, Options: v.Options, Labels: v.Labels}
}

// ListVolumes implements Docker.
func (d *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	res, err := d.c.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, fromVolume(v))
	}
	return out, nil
}

// VolumeSizes implements Docker via the disk-usage endpoint.
func (d *Client) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	res, err := d.c.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true})
	if err != nil {
		return nil, fmt.Errorf("disk usage: %w", err)
	}
	sizes := make(map[string]int64, len(res.Volumes.Items))
	for _, v := range res.Volumes.Items {
		if v.UsageData != nil {
			sizes[v.Name] = v.UsageData.Size
		}
	}
	return sizes, nil
}

// InspectVolume implements Docker.
func (d *Client) InspectVolume(ctx context.Context, name string) (Volume, bool, error) {
	res, err := d.c.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return Volume{}, false, nil
	}
	if err != nil {
		return Volume{}, false, fmt.Errorf("inspect volume %s: %w", name, err)
	}
	return fromVolume(res.Volume), true, nil
}

// CreateVolume implements Docker.
func (d *Client) CreateVolume(ctx context.Context, v Volume) error {
	_, err := d.c.VolumeCreate(ctx, client.VolumeCreateOptions{Name: v.Name, Driver: v.Driver, DriverOpts: v.Options, Labels: v.Labels})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", v.Name, err)
	}
	return nil
}

// ListContainers implements Docker.
func (d *Client) ListContainers(ctx context.Context) ([]Container, error) {
	res, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		name := s.ID[:min(12, len(s.ID))]
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		mounts := make([]Mount, 0, len(s.Mounts))
		for _, m := range s.Mounts {
			mounts = append(mounts, Mount{Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
		}
		out = append(out, Container{ID: s.ID, Name: name, Image: s.Image, State: string(s.State), Labels: s.Labels, Mounts: mounts})
	}
	return out, nil
}

// StartedAt implements Docker. A container that never started yields the zero time.
func (d *Client) StartedAt(ctx context.Context, id string) (time.Time, error) {
	res, err := d.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return time.Time{}, fmt.Errorf("inspect container %s: %w", id, err)
	}
	if res.Container.State == nil {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, res.Container.State.StartedAt)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// StopContainer implements Docker using the container's own stop timeout.
func (d *Client) StopContainer(ctx context.Context, id string) error {
	if _, err := d.c.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil {
		return err
	}
	return nil
}

// StartContainer implements Docker.
func (d *Client) StartContainer(ctx context.Context, id string) error {
	if _, err := d.c.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return err
	}
	return nil
}

// EnsureImage implements Docker. Pulls are anonymous.
func (d *Client) EnsureImage(ctx context.Context, ref string) (string, error) {
	insp, err := d.c.ImageInspect(ctx, ref)
	if cerrdefs.IsNotFound(err) {
		resp, perr := d.c.ImagePull(ctx, ref, client.ImagePullOptions{})
		if perr != nil {
			return "", pullError(ref, perr)
		}
		werr := resp.Wait(ctx)
		resp.Close()
		if werr != nil {
			return "", pullError(ref, werr)
		}
		insp, err = d.c.ImageInspect(ctx, ref)
	}
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", ref, err)
	}
	if len(insp.RepoDigests) > 0 {
		return insp.RepoDigests[0], nil
	}
	return insp.ID, nil
}

func pullError(ref string, err error) error {
	return fmt.Errorf("pull helper image %s: %w (dv-backup pulls anonymously; for a private registry run `docker pull %s` first)", ref, err, ref)
}

// RunHelper is implemented in helper.go.
var _ Docker = (*Client)(nil)

var errNotImplemented = errors.New("not implemented")

// RunHelper is replaced in Task 8.
func (d *Client) RunHelper(context.Context, Helper) (HelperResult, error) { return HelperResult{}, errNotImplemented }
