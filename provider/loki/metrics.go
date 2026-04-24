// Package loki implements iface.MetricsBackend by pushing JSON log entries
// to the Grafana Loki HTTP push API.
//
// The JSON field names and stream labels must match the Grafana dashboard
// queries exactly, or the dashboard breaks.
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// Backend pushes metrics to Grafana Cloud Loki.
type Backend struct {
	pushURL  string
	username string
	apiKey   string
	org      string
	client   *http.Client
}

// New creates a Loki metrics backend.
func New(pushURL, username, apiKey, org string) *Backend {
	return &Backend{
		pushURL:  pushURL,
		username: username,
		apiKey:   apiKey,
		org:      org,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// PushRunnerMetrics pushes runner pool state with stream labels matching the dashboard.
func (b *Backend) PushRunnerMetrics(ctx context.Context, m domain.RunnerMetrics) error {
	labels := map[string]string{
		"job":     "gh-runner-scaler",
		"service": "runner-metrics",
		"org":     b.org,
	}
	return b.push(ctx, labels, m)
}

// PushWorkflowMetrics pushes workflow run data as individual log entries.
func (b *Backend) PushWorkflowMetrics(ctx context.Context, runs []domain.WorkflowMetrics) error {
	if len(runs) == 0 {
		return nil
	}
	labels := map[string]string{
		"job":     "gh-runner-scaler",
		"service": "workflow-metrics",
		"org":     b.org,
	}
	entries := make([]any, 0, len(runs))
	for _, run := range runs {
		entries = append(entries, run)
	}
	return b.pushEntries(ctx, labels, entries)
}

// PushHostMetrics pushes container and storage pool state.
func (b *Backend) PushHostMetrics(ctx context.Context, m domain.HostMetrics) error {
	labels := map[string]string{
		"job":     "gh-runner-scaler",
		"service": "host-metrics",
		"org":     b.org,
	}
	return b.push(ctx, labels, m)
}

// lokiPayload matches the Loki push API format.
type lokiPayload struct {
	Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

func (b *Backend) push(ctx context.Context, labels map[string]string, data any) error {
	return b.pushEntries(ctx, labels, []any{data})
}

func (b *Backend) pushEntries(ctx context.Context, labels map[string]string, entries []any) error {
	if len(entries) == 0 {
		return nil
	}

	nowNS := time.Now().UnixNano()
	values := make([][]string, 0, len(entries))
	for i, entry := range entries {
		valueJSON, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshaling metrics: %w", err)
		}
		values = append(values, []string{
			strconv.FormatInt(nowNS+int64(i), 10),
			string(valueJSON),
		})
	}

	payload := lokiPayload{
		Streams: []lokiStream{{
			Stream: labels,
			Values: values,
		}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling Loki payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating Loki request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(b.username, b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("pushing to Loki: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Loki push returned %d", resp.StatusCode)
	}
	return nil
}
