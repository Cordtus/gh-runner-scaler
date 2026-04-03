// Package lxd implements iface.ContainerRuntime and iface.CacheManager
// via the LXD REST API using the canonical/lxd Go client.
package lxd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	lxdclient "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// Runtime implements ContainerRuntime via the LXD API.
type Runtime struct {
	server   lxdclient.InstanceServer
	template string
	remote   string // stored for logging; the server connection already targets the remote
}

// New connects to a local or remote LXD daemon and returns a Runtime.
func New(socket, remote, remoteURL, template string) (*Runtime, error) {
	var server lxdclient.InstanceServer
	var err error

	if remoteURL != "" {
		// Remote HTTPS connection
		server, err = lxdclient.ConnectLXD(remoteURL, &lxdclient.ConnectionArgs{})
		if err != nil {
			return nil, fmt.Errorf("connecting to remote LXD at %s: %w", remoteURL, err)
		}
	} else {
		// Local Unix socket connection
		socketPath := socket
		if socketPath == "" {
			socketPath = "/var/snap/lxd/common/lxd/unix.socket"
		}
		server, err = lxdclient.ConnectLXDUnix(socketPath, nil)
		if err != nil {
			return nil, fmt.Errorf("connecting to local LXD at %s: %w", socketPath, err)
		}
	}

	return &Runtime{
		server:   server,
		template: template,
		remote:   remote,
	}, nil
}

// CloneFromTemplate creates a new container by copying the stopped template.
// On ZFS with the template on the same pool, this is a metadata-only clone (~0.4s).
func (r *Runtime) CloneFromTemplate(ctx context.Context, name string) error {
	source, _, err := r.server.GetInstance(r.template)
	if err != nil {
		return fmt.Errorf("getting template %s: %w", r.template, err)
	}

	req := lxdclient.InstanceCopyArgs{
		Name: name,
		Mode: "pull",
	}

	op, err := r.server.CopyInstance(r.server, *source, &req)
	if err != nil {
		return fmt.Errorf("copying template to %s: %w", name, err)
	}

	// CopyInstance returns a RemoteOperation; use Wait (not WaitContext).
	if err := op.Wait(); err != nil {
		return fmt.Errorf("waiting for copy of %s: %w", name, err)
	}
	return nil
}

// StartContainer boots a stopped container.
func (r *Runtime) StartContainer(ctx context.Context, name string) error {
	reqState := api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err := r.server.UpdateInstanceState(name, reqState, "")
	if err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return op.WaitContext(ctx)
}

// StopContainer force-stops a running container.
func (r *Runtime) StopContainer(ctx context.Context, name string) error {
	reqState := api.InstanceStatePut{
		Action:  "stop",
		Timeout: -1,
		Force:   true,
	}
	op, err := r.server.UpdateInstanceState(name, reqState, "")
	if err != nil {
		return fmt.Errorf("stopping %s: %w", name, err)
	}
	return op.WaitContext(ctx)
}

// DeleteContainer removes a container entirely.
func (r *Runtime) DeleteContainer(ctx context.Context, name string) error {
	op, err := r.server.DeleteInstance(name, false)
	if err != nil {
		return fmt.Errorf("deleting %s: %w", name, err)
	}
	return op.WaitContext(ctx)
}

// ExecCommand runs a command inside a running container, returning combined stdout/stderr.
func (r *Runtime) ExecCommand(_ context.Context, name string, cmd []string) (string, error) {
	var stdout, stderr bytes.Buffer

	req := api.InstanceExecPost{
		Command:     cmd,
		WaitForWS:   true,
		Interactive: false,
	}

	args := lxdclient.InstanceExecArgs{
		Stdout: &stdout,
		Stderr: &stderr,
	}

	op, err := r.server.ExecInstance(name, req, &args)
	if err != nil {
		return "", fmt.Errorf("exec in %s: %w", name, err)
	}

	if err := op.Wait(); err != nil {
		return "", fmt.Errorf("exec in %s failed: %w (stderr: %s)", name, err, stderr.String())
	}

	// Check exit code from operation metadata
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		return stdout.String(), fmt.Errorf("exec in %s returned non-zero (stderr: %s)", name, stderr.String())
	}

	return stdout.String(), nil
}

// WaitForReady polls until the check command succeeds or the timeout expires.
func (r *Runtime) WaitForReady(ctx context.Context, name string, check []string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%s did not become ready within %s", name, timeout)
		case <-ticker.C:
			_, err := r.ExecCommand(ctx, name, check)
			if err == nil {
				return nil
			}
		}
	}
}

// ListContainers returns all containers whose names start with prefix.
func (r *Runtime) ListContainers(_ context.Context, prefix string) ([]domain.Container, error) {
	instances, err := r.server.GetInstances(lxdclient.GetInstancesArgs{InstanceType: api.InstanceTypeContainer})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var result []domain.Container
	for _, inst := range instances {
		if !strings.HasPrefix(inst.Name, prefix) {
			continue
		}
		result = append(result, domain.Container{
			Name:   inst.Name,
			Status: mapStatus(inst.Status),
		})
	}
	return result, nil
}

// GetContainerStatus returns the current status of a single container.
func (r *Runtime) GetContainerStatus(_ context.Context, name string) (domain.ContainerStatus, error) {
	state, _, err := r.server.GetInstanceState(name)
	if err != nil {
		return domain.StatusUnknown, fmt.Errorf("getting status of %s: %w", name, err)
	}
	return mapStatus(state.Status), nil
}

// HostMetrics collects container counts and storage pool usage for metrics.
func (r *Runtime) HostMetrics(cachePool string) (domain.HostMetrics, error) {
	instances, err := r.server.GetInstances(lxdclient.GetInstancesArgs{InstanceType: api.InstanceTypeContainer})
	if err != nil {
		return domain.HostMetrics{}, fmt.Errorf("listing containers: %w", err)
	}

	var m domain.HostMetrics
	for _, inst := range instances {
		switch mapStatus(inst.Status) {
		case domain.StatusRunning:
			m.ContainersRunning++
		case domain.StatusStopped:
			m.ContainersStopped++
		}
	}

	if cachePool != "" {
		resources, err := r.server.GetStoragePoolResources(cachePool)
		if err == nil && resources.Space.Total > 0 {
			m.CachePoolUsedGB = float64(resources.Space.Used) / (1024 * 1024 * 1024)
			m.CachePoolTotalGB = float64(resources.Space.Total) / (1024 * 1024 * 1024)
			m.CachePoolPct = float64(resources.Space.Used) / float64(resources.Space.Total) * 100
		}
	}

	return m, nil
}

func mapStatus(s string) domain.ContainerStatus {
	switch strings.ToLower(s) {
	case "running":
		return domain.StatusRunning
	case "stopped":
		return domain.StatusStopped
	default:
		return domain.StatusUnknown
	}
}

// Ensure stdout/stderr satisfy io.Writer at compile time.
var _ io.Writer = (*bytes.Buffer)(nil)
