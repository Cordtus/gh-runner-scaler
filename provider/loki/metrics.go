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
	"sort"
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
	baseLabels := map[string]string{
		"job":     "gh-runner-scaler",
		"service": "workflow-metrics",
		"org":     b.org,
	}

	repoRuns := make(map[string][]domain.WorkflowMetrics)
	for _, run := range runs {
		repoRuns[run.Repo] = append(repoRuns[run.Repo], run)
	}

	repos := make([]string, 0, len(repoRuns))
	for repo := range repoRuns {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	streams := make([]lokiStream, 0, len(repos))
	for _, repo := range repos {
		runs := append([]domain.WorkflowMetrics(nil), repoRuns[repo]...)
		sortWorkflowMetrics(runs)

		labels := cloneLabels(baseLabels)
		if repo != "" {
			labels["repo"] = repo
		}

		entries := make([]any, 0, len(runs))
		for _, run := range runs {
			entries = append(entries, run)
		}

		values, err := buildValues(entries)
		if err != nil {
			return err
		}
		streams = append(streams, lokiStream{
			Stream: labels,
			Values: values,
		})
	}

	return b.pushPayload(ctx, lokiPayload{Streams: streams})
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

// PushIssueEvents pushes warning/error events for dashboarding.
func (b *Backend) PushIssueEvents(ctx context.Context, m []domain.IssueEvent) error {
	if len(m) == 0 {
		return nil
	}
	labels := map[string]string{
		"job":     "gh-runner-scaler",
		"service": "issue-events",
		"org":     b.org,
	}
	entries := make([]any, 0, len(m))
	for _, issue := range m {
		entries = append(entries, issue)
	}
	return b.pushEntries(ctx, labels, entries)
}

// PushLifecycleMetrics pushes aggregate autoscaling behavior snapshots.
func (b *Backend) PushLifecycleMetrics(ctx context.Context, m domain.LifecycleMetrics) error {
	labels := map[string]string{
		"job":     "gh-runner-scaler",
		"service": "lifecycle-metrics",
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

	values, err := buildValues(entries)
	if err != nil {
		return err
	}

	return b.pushPayload(ctx, lokiPayload{
		Streams: []lokiStream{{
			Stream: labels,
			Values: values,
		}},
	})
}

func buildValues(entries []any) ([][]string, error) {
	values := make([][]string, 0, len(entries))
	for i, entry := range entries {
		valueJSON, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("marshaling metrics: %w", err)
		}
		timestamp := entryTimestamp(entry)
		if timestamp.IsZero() {
			timestamp = time.Now()
		}
		values = append(values, []string{
			strconv.FormatInt(timestamp.UTC().UnixNano()+int64(i), 10),
			string(valueJSON),
		})
	}
	return values, nil
}

func (b *Backend) pushPayload(ctx context.Context, payload lokiPayload) error {
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

func entryTimestamp(entry any) time.Time {
	switch value := entry.(type) {
	case domain.WorkflowMetrics:
		return parseEntryTime(value.CompletedAt)
	case domain.IssueEvent:
		return parseEntryTime(value.ObservedAt)
	default:
		return time.Time{}
	}
}

func parseEntryTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func sortWorkflowMetrics(runs []domain.WorkflowMetrics) {
	sort.SliceStable(runs, func(i, j int) bool {
		left := parseEntryTime(runs[i].CompletedAt)
		right := parseEntryTime(runs[j].CompletedAt)

		switch {
		case left.IsZero() && right.IsZero():
			return workflowMetricTieBreak(runs[i], runs[j])
		case left.IsZero():
			return false
		case right.IsZero():
			return true
		case left.Equal(right):
			return workflowMetricTieBreak(runs[i], runs[j])
		default:
			return left.Before(right)
		}
	})
}

func workflowMetricTieBreak(left, right domain.WorkflowMetrics) bool {
	if left.RunID != right.RunID {
		return left.RunID < right.RunID
	}
	if left.RunAttempt != right.RunAttempt {
		return left.RunAttempt < right.RunAttempt
	}
	if left.RunNumber != right.RunNumber {
		return left.RunNumber < right.RunNumber
	}
	if left.Workflow != right.Workflow {
		return left.Workflow < right.Workflow
	}
	return left.Branch < right.Branch
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}
