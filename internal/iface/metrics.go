package iface

import (
	"context"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// MetricsBackend abstracts pushing structured metrics to an observability platform.
// The Loki provider implements this via HTTP POST; future providers
// could target Prometheus pushgateway, Datadog, etc.
type MetricsBackend interface {
	// PushRunnerMetrics pushes runner pool state.
	PushRunnerMetrics(ctx context.Context, m domain.RunnerMetrics) error

	// PushWorkflowMetrics pushes recent workflow run data.
	PushWorkflowMetrics(ctx context.Context, m []domain.WorkflowMetrics) error

	// PushHostMetrics pushes container count and storage pool state.
	PushHostMetrics(ctx context.Context, m domain.HostMetrics) error
}
