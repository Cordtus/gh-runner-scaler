package iface

import (
	"context"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// CIProvider abstracts CI platform operations (runner management, webhooks).
// The GitHub provider implements this via go-github; future providers
// could target GitLab, Gitea, etc.
type CIProvider interface {
	// ListRunners returns all runners registered with the configured target.
	ListRunners(ctx context.Context) ([]domain.Runner, error)

	// GetRegistrationToken returns a short-lived token for registering a new runner.
	GetRegistrationToken(ctx context.Context) (string, error)

	// GetRemoveToken returns a short-lived token for deregistering a runner.
	GetRemoveToken(ctx context.Context) (string, error)

	// DeleteRunner removes a runner by ID from the CI platform.
	DeleteRunner(ctx context.Context, runnerID int64) error

	// RegistrationURL returns the URL used in runner config (e.g. https://github.com/OrgName or https://github.com/owner/repo).
	RegistrationURL() string

	// ClassifyRunner returns true if the runner name matches the auto-scaled prefix.
	ClassifyRunner(name string) bool

	// ValidateWebhookPayload verifies the webhook signature against the shared secret.
	ValidateWebhookPayload(payload []byte, signature string) error

	// ParseWebhookEvent converts a raw webhook payload into a provider-agnostic event.
	ParseWebhookEvent(eventType string, payload []byte) (*domain.WebhookEvent, error)

	// ListRecentWorkflowRuns returns completed workflow runs for metrics collection.
	ListRecentWorkflowRuns(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error)

	// EnrichWorkflowMetrics hydrates optional details for a batch of fresh workflow metrics.
	// Providers should preserve the input ordering and return the original runs when no
	// enrichment is available.
	EnrichWorkflowMetrics(ctx context.Context, runs []domain.WorkflowMetrics) ([]domain.WorkflowMetrics, error)
}

// RunnerInventoryMetricsProvider is an optional CI extension for dashboards.
// It may return bounded stale inventory with metadata; reconcile must keep using
// CIProvider.ListRunners for fresh authoritative state.
type RunnerInventoryMetricsProvider interface {
	ListRunnersForMetrics(ctx context.Context) ([]domain.Runner, domain.RunnerInventoryMeta, error)
}
