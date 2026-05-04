package lxd

import (
	"testing"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestHostMetricsFromContainers_UsesProvidedSnapshot(t *testing.T) {
	runtime := &Runtime{}

	metrics, err := runtime.HostMetricsFromContainers("", []domain.Container{
		{Name: "auto-1", Status: domain.StatusRunning},
		{Name: "auto-2", Status: domain.StatusStopped},
		{Name: "auto-3", Status: domain.StatusUnknown},
	})
	if err != nil {
		t.Fatalf("HostMetricsFromContainers returned error: %v", err)
	}
	if metrics.ContainersRunning != 1 {
		t.Fatalf("ContainersRunning = %d, want 1", metrics.ContainersRunning)
	}
	if metrics.ContainersStopped != 1 {
		t.Fatalf("ContainersStopped = %d, want 1", metrics.ContainersStopped)
	}
}
