// Package lxd implements iface.ContainerRuntime and iface.CacheManager
// via the LXD REST API using the canonical/lxd Go client.
package lxd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
//
// Connection priority:
//  1. If remoteURL is set, connect over HTTPS with TLS client certs.
//  2. If remote is set (e.g. "nodev2"), look up the address from the
//     standard LXD client config at ~/.config/lxc/config.yml.
//  3. Otherwise, connect to the local Unix socket.
//
// TLS certs are read from certPath/keyPath if provided, or from the
// standard LXD client config directory (~/.config/lxc/).
func New(socket, remote, remoteURL, certPath, keyPath, template string) (*Runtime, error) {
	var server lxdclient.InstanceServer
	var err error

	switch {
	case remoteURL != "":
		// Explicit remote URL -- use TLS client certs.
		server, err = connectRemote(remoteURL, certPath, keyPath, serverCertPathForRemote(remote))
		if err != nil {
			return nil, fmt.Errorf("connecting to remote LXD at %s: %w", remoteURL, err)
		}

	case remote != "":
		// Named remote -- resolve from the LXD client config.
		addr, err := resolveRemoteAddr(remote)
		if err != nil {
			return nil, fmt.Errorf("resolving remote %q: %w", remote, err)
		}
		server, err = connectRemote(addr, certPath, keyPath, serverCertPathForRemote(remote))
		if err != nil {
			return nil, fmt.Errorf("connecting to remote %q at %s: %w", remote, addr, err)
		}

	default:
		// Local Unix socket.
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

// connectRemote connects to a remote LXD daemon over HTTPS using
// TLS client certs from explicit paths or the standard LXD config directory.
func connectRemote(addr, certPath, keyPath, serverCertPath string) (lxdclient.InstanceServer, error) {
	configDir := lxdConfigDir()
	if certPath == "" {
		certPath = filepath.Join(configDir, "client.crt")
	}
	if keyPath == "" {
		keyPath = filepath.Join(configDir, "client.key")
	}

	clientCert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("reading client cert %s: %w", certPath, err)
	}
	clientKey, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading client key %s: %w", keyPath, err)
	}

	serverCert := ""
	if serverCertPath != "" {
		data, err := os.ReadFile(serverCertPath)
		switch {
		case err == nil:
			serverCert = string(data)
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("reading server cert %s: %w", serverCertPath, err)
		}
	}

	return lxdclient.ConnectLXD(addr, &lxdclient.ConnectionArgs{
		TLSClientCert: string(clientCert),
		TLSClientKey:  string(clientKey),
		TLSServerCert: serverCert,
	})
}

func serverCertPathForRemote(remote string) string {
	if remote == "" {
		return ""
	}
	return filepath.Join(lxdConfigDir(), "servercerts", remote+".crt")
}

// resolveRemoteAddr looks up a named remote's address from the LXD client
// config file at ~/.config/lxc/config.yml. This is a minimal YAML parser
// that avoids pulling in a YAML dependency just for this lookup.
func resolveRemoteAddr(name string) (string, error) {
	configPath := filepath.Join(lxdConfigDir(), "config.yml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("reading LXD config %s: %w", configPath, err)
	}

	// Simple line-by-line parse: find "  <name>:" then next "    addr: <value>"
	lines := strings.Split(string(data), "\n")
	inRemote := false
	needle := "  " + name + ":"
	for _, line := range lines {
		if strings.TrimRight(line, " \t") == needle {
			inRemote = true
			continue
		}
		if inRemote {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "addr:") {
				addr := strings.TrimSpace(strings.TrimPrefix(trimmed, "addr:"))
				if addr == "" || addr == "unix://" {
					return "", fmt.Errorf("remote %q has no HTTPS address", name)
				}
				return addr, nil
			}
			// If we hit another remote definition, stop.
			if !strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "\t\t") && strings.TrimSpace(line) != "" {
				break
			}
		}
	}
	return "", fmt.Errorf("remote %q not found in %s", name, configPath)
}

// lxdConfigDir returns the standard LXD client config directory.
func lxdConfigDir() string {
	if dir := os.Getenv("LXD_CONF"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/root", ".config", "lxc")
	}
	return filepath.Join(home, ".config", "lxc")
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

	// Clear the inherited MAC address so LXD generates a fresh one.
	// The MAC lives in volatile config (volatile.eth0.hwaddr), not in
	// the device config. Without clearing it, LXD refuses to start the
	// clone ("MAC address already defined on another NIC").
	inst, etag, err := r.server.GetInstance(name)
	if err != nil {
		return fmt.Errorf("getting cloned instance %s: %w", name, err)
	}

	changed := false

	// Clear volatile MAC entries for all NICs.
	for key := range inst.Config {
		if strings.HasSuffix(key, ".hwaddr") && strings.HasPrefix(key, "volatile.") {
			delete(inst.Config, key)
			changed = true
		}
	}

	// Also clear hwaddr from device config if present.
	for devName, dev := range inst.Devices {
		if _, ok := dev["hwaddr"]; ok {
			delete(dev, "hwaddr")
			inst.Devices[devName] = dev
			changed = true
		}
	}

	if changed {
		updateOp, err := r.server.UpdateInstance(name, inst.Writable(), etag)
		if err != nil {
			return fmt.Errorf("clearing MAC on %s: %w", name, err)
		}
		if err := updateOp.WaitContext(ctx); err != nil {
			return fmt.Errorf("waiting for MAC clear on %s: %w", name, err)
		}
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
	return waitOperation(ctx, op)
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
	return waitOperation(ctx, op)
}

// DeleteContainer removes a container entirely.
func (r *Runtime) DeleteContainer(ctx context.Context, name string) error {
	op, err := r.server.DeleteInstance(name, false)
	if err != nil {
		return fmt.Errorf("deleting %s: %w", name, err)
	}
	return waitOperation(ctx, op)
}

// ExecCommand runs a command inside a running container, returning combined stdout/stderr.
func (r *Runtime) ExecCommand(ctx context.Context, name string, cmd []string) (string, error) {
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

	if err := waitOperation(ctx, op); err != nil {
		return "", fmt.Errorf("exec in %s failed: %w (stderr: %s)", name, err, stderr.String())
	}

	// Check exit code from operation metadata
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		return stdout.String(), fmt.Errorf("exec in %s returned non-zero (stderr: %s)", name, stderr.String())
	}

	return stdout.String(), nil
}

func waitOperation(ctx context.Context, op lxdclient.Operation) error {
	if ctx == nil {
		ctx = context.Background()
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = op.Cancel()
		case <-done:
		}
	}()
	defer close(done)

	return op.WaitContext(ctx)
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
