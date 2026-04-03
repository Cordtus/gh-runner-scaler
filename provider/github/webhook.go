package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

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
	var event struct {
		Action      string `json:"action"`
		WorkflowJob struct {
			Name string `json:"name"`
		} `json:"workflow_job"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("parsing workflow_job: %w", err)
	}

	var evType domain.WebhookEventType
	switch event.Action {
	case "queued":
		evType = domain.EventJobQueued
	case "completed":
		evType = domain.EventJobCompleted
	default:
		return nil, nil // other actions (in_progress, etc.) are ignored
	}

	return &domain.WebhookEvent{
		Type:   evType,
		Repo:   event.Repository.FullName,
		Detail: fmt.Sprintf("%s: %s / %s", event.Action, event.Repository.FullName, event.WorkflowJob.Name),
	}, nil
}

func parsePush(payload []byte) (*domain.WebhookEvent, error) {
	var event struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("parsing push: %w", err)
	}

	short := event.After
	if len(short) > 7 {
		short = short[:7]
	}

	return &domain.WebhookEvent{
		Type:   domain.EventPush,
		Repo:   event.Repository.FullName,
		Ref:    event.Ref,
		Detail: fmt.Sprintf("push %s to %s (%s)", event.Ref, event.Repository.FullName, short),
	}, nil
}
