package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v74/github"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// WebhookValidator handles GitHub webhook signature verification and event parsing.
type WebhookValidator struct {
	secret string
}

// NewWebhookValidator creates a validator with the given webhook secret.
func NewWebhookValidator(secret string) *WebhookValidator {
	return &WebhookValidator{secret: secret}
}

// SetValidator attaches a webhook validator to the provider.
func (p *Provider) SetValidator(secret string) {
	p.validator = NewWebhookValidator(secret)
}

func init() {
	// Ensure Provider has the validator field (set at construction time via SetValidator).
}

// ValidateWebhookPayload verifies the HMAC-SHA256 signature.
func (p *Provider) ValidateWebhookPayload(payload []byte, signature string) error {
	if p.validator == nil {
		return fmt.Errorf("webhook validator not configured")
	}
	return p.validator.Validate(payload, signature)
}

// Validate checks the payload signature against the shared secret.
func (v *WebhookValidator) Validate(payload []byte, signature string) error {
	if v.secret == "" {
		return fmt.Errorf("webhook secret not configured")
	}

	sig := strings.TrimPrefix(signature, "sha256=")
	if sig == signature {
		return fmt.Errorf("unsupported signature format (expected sha256=...)")
	}

	mac := hmac.New(sha256.New, []byte(v.secret))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// ParseWebhookEvent converts a raw GitHub webhook payload into a domain event.
func (p *Provider) ParseWebhookEvent(eventType string, payload []byte) (*domain.WebhookEvent, error) {
	switch eventType {
	case "workflow_job":
		return parseWorkflowJob(payload)
	case "push":
		return parsePush(payload)
	default:
		return nil, nil // unknown event types are silently ignored
	}
}

func parseWorkflowJob(payload []byte) (*domain.WebhookEvent, error) {
	parsed, err := gh.ParseWebHook("workflow_job", payload)
	if err != nil {
		return nil, fmt.Errorf("parsing workflow_job: %w", err)
	}
	event, ok := parsed.(*gh.WorkflowJobEvent)
	if !ok {
		return nil, fmt.Errorf("parsing workflow_job: unexpected payload type %T", parsed)
	}

	var evType domain.WebhookEventType
	switch event.GetAction() {
	case "queued":
		evType = domain.EventJobQueued
	case "completed":
		evType = domain.EventJobCompleted
	case "in_progress":
		evType = domain.EventUnknown
	default:
		return nil, nil // other actions are ignored
	}

	job := event.GetWorkflowJob()
	labels := append([]string(nil), job.Labels...)
	commit := job.GetHeadSHA()
	shortCommit := commit
	if len(shortCommit) > 7 {
		shortCommit = shortCommit[:7]
	}
	detail := fmt.Sprintf("%s: %s / %s", event.GetAction(), event.GetRepo().GetFullName(), job.GetName())
	if shortCommit != "" {
		detail += fmt.Sprintf(" (%s)", shortCommit)
	}
	if runner := job.GetRunnerName(); runner != "" {
		detail += " runner=" + runner
	}

	return &domain.WebhookEvent{
		Type:       evType,
		EventType:  "workflow_job",
		Action:     event.GetAction(),
		Repo:       event.GetRepo().GetFullName(),
		Branch:     job.GetHeadBranch(),
		Commit:     commit,
		Workflow:   job.GetWorkflowName(),
		Job:        job.GetName(),
		JobID:      job.GetID(),
		Runner:     job.GetRunnerName(),
		Status:     job.GetStatus(),
		Conclusion: job.GetConclusion(),
		RunID:      job.GetRunID(),
		RunAttempt: int(job.GetRunAttempt()),
		Detail:     detail,
		Labels:     labels,
	}, nil
}

func parsePush(payload []byte) (*domain.WebhookEvent, error) {
	parsed, err := gh.ParseWebHook("push", payload)
	if err != nil {
		return nil, fmt.Errorf("parsing push: %w", err)
	}
	event, ok := parsed.(*gh.PushEvent)
	if !ok {
		return nil, fmt.Errorf("parsing push: unexpected payload type %T", parsed)
	}

	short := event.GetAfter()
	if len(short) > 7 {
		short = short[:7]
	}
	branch := strings.TrimPrefix(event.GetRef(), "refs/heads/")

	return &domain.WebhookEvent{
		Type:          domain.EventPush,
		EventType:     "push",
		Action:        "push",
		Repo:          event.GetRepo().GetFullName(),
		Ref:           event.GetRef(),
		DefaultBranch: event.GetRepo().GetDefaultBranch(),
		Branch:        branch,
		Commit:        event.GetAfter(),
		Detail:        fmt.Sprintf("push %s to %s (%s)", event.GetRef(), event.GetRepo().GetFullName(), short),
	}, nil
}
