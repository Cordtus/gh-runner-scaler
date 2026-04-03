package iface

import (
	"context"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// CIProvider abstracts CI platform operations (runner management, webhooks).
// The GitHub provider implements this via go-github; future providers
// could target GitLab, Gitea, etc.
type CIProvider interface {
	// ListRunners returns all runners registered with the org/group.
	ListRunners(ctx context.Context) ([]domain.Runner, error)

	// GetRegistrationToken returns a short-lived token for registering a new runner.
	GetRegistrationToken(ctx context.Context) (string, error)

	// GetRemoveToken returns a short-lived token for deregistering a runner.
	GetRemoveToken(ctx context.Context) (string, error)

	// DeleteRunner removes a runner by ID from the CI platform.
	DeleteRunner(ctx context.Context, runnerID int64) error

	// RegistrationURL returns the URL used in runner config (e.g. https://github.com/OrgName).
	RegistrationURL() string

	// ClassifyRunner returns true if the runner name matches the auto-scaled prefix.
	ClassifyRunner(name string) bool

	// ValidateWebhookPayload verifies the webhook signature against the shared secret.
	ValidateWebhookPayload(payload []byte, signature string) error

	// ParseWebhookEvent converts a raw webhook payload into a provider-agnostic event.
	ParseWebhookEvent(eventType string, payload []byte) (*domain.WebhookEvent, error)

	// ListRecentWorkflowRuns returns completed workflow runs for metrics collection.
	ListRecentWorkflowRuns(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error)
}
