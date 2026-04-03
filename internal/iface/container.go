// Package iface defines the provider interfaces that the engine depends on.
// No file in this package imports concrete implementations.
package iface

import (
	"context"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// ContainerRuntime abstracts container lifecycle operations.
// The LXD provider implements this via the LXD REST API; future providers
// could target Docker, GCE instances, etc.
type ContainerRuntime interface {
	// CloneFromTemplate creates a new container by cloning the configured template.
	CloneFromTemplate(ctx context.Context, name string) error

	// StartContainer boots a stopped container.
	StartContainer(ctx context.Context, name string) error

	// StopContainer force-stops a running container.
	StopContainer(ctx context.Context, name string) error

	// DeleteContainer removes a container entirely.
	DeleteContainer(ctx context.Context, name string) error

	// ExecCommand runs a command inside a running container and returns stdout.
	ExecCommand(ctx context.Context, name string, cmd []string) (string, error)

	// WaitForReady polls until the check command succeeds or the timeout expires.
	WaitForReady(ctx context.Context, name string, check []string, timeout time.Duration) error

	// ListContainers returns all containers whose names start with prefix.
	ListContainers(ctx context.Context, prefix string) ([]domain.Container, error)

	// GetContainerStatus returns the current status of a single container.
	GetContainerStatus(ctx context.Context, name string) (domain.ContainerStatus, error)
}
