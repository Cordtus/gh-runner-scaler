package daemon

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

type runnerLifecycle struct {
	jobs map[string]struct{}
}

func (d *Daemon) collectLogDerivedMetrics(ctx context.Context) {
	if d.metrics == nil || d.logStore == nil {
		return
	}

	entries := d.logStore.Snapshot()
	lifecycle := buildLifecycleMetrics(entries)
	if err := d.metrics.PushLifecycleMetrics(ctx, lifecycle); err != nil {
		d.log.Error("failed to push lifecycle metrics", "error", err)
	}

	issueEntries := d.filterNewIssueEntries(selectIssueLogEntries(entries))
	if len(issueEntries) == 0 {
		return
	}

	issues := make([]domain.IssueEvent, 0, len(issueEntries))
	for _, entry := range issueEntries {
		issues = append(issues, buildIssueEvent(entry))
	}

	if err := d.metrics.PushIssueEvents(ctx, issues); err != nil {
		d.log.Error("failed to push issue events", "error", err)
		return
	}
	d.markIssueEntriesDelivered(issueEntries)
}

func selectIssueLogEntries(entries []domain.LogEntry) []domain.LogEntry {
	if len(entries) == 0 {
		return nil
	}

	issues := make([]domain.LogEntry, 0, len(entries))
	for _, entry := range entries {
		level := strings.ToUpper(strings.TrimSpace(entry.Level))
		if level != "WARN" && level != "ERROR" {
			continue
		}
		issues = append(issues, entry)
	}
	return issues
}

func buildIssueEvent(entry domain.LogEntry) domain.IssueEvent {
	return domain.IssueEvent{
		Level:      strings.ToLower(entry.Level),
		Kind:       "daemon",
		Reason:     deriveIssueReason(entry),
		Message:    entry.Message,
		Error:      entry.Error,
		Detail:     entry.Detail,
		EventType:  entry.EventType,
		Action:     entry.Action,
		Repo:       entry.Repo,
		Branch:     entry.Branch,
		Workflow:   entry.Workflow,
		Job:        entry.Job,
		JobID:      entry.JobID,
		Runner:     entry.Runner,
		Container:  entry.Container,
		RunID:      entry.RunID,
		RunAttempt: entry.RunAttempt,
		Conclusion: entry.Conclusion,
		ObservedAt: entry.Time.UTC().Format(time.RFC3339),
	}
}

func deriveIssueReason(entry domain.LogEntry) string {
	for _, candidate := range []string{entry.Detail, entry.Error, entry.Message} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		candidate = strings.Join(strings.Fields(candidate), " ")
		if len(candidate) > 160 {
			candidate = candidate[:157] + "..."
		}
		return candidate
	}
	return "unknown"
}

func buildLifecycleMetrics(entries []domain.LogEntry) domain.LifecycleMetrics {
	metrics := domain.LifecycleMetrics{}
	if len(entries) == 0 {
		return metrics
	}

	metrics.WindowStart = entries[0].Time.UTC().Format(time.RFC3339)
	metrics.WindowEnd = entries[len(entries)-1].Time.UTC().Format(time.RFC3339)

	queueStarts := make(map[string]time.Time)
	activeLifecycles := make(map[string]*runnerLifecycle)
	var queueWaits []float64
	var lifecycleJobCounts []float64
	var scaleGaps []float64
	var lastScaleDownAt time.Time

	for _, entry := range entries {
		switch {
		case entry.EventType == "workflow_job" && entry.Action == "queued":
			queueStarts[analyticsJobKey(entry)] = entry.Time
		case entry.EventType == "workflow_job" && entry.Action == "in_progress":
			key := analyticsJobKey(entry)
			if queuedAt, ok := queueStarts[key]; ok && !entry.Time.Before(queuedAt) {
				queueWaits = append(queueWaits, entry.Time.Sub(queuedAt).Seconds())
				delete(queueStarts, key)
			}
			if entry.Runner != "" {
				if lifecycle := activeLifecycles[entry.Runner]; lifecycle != nil {
					lifecycle.jobs[key] = struct{}{}
				}
			}
		case entry.EventType == "scale_up" && entry.Action == "completed" && entry.Runner != "":
			activeLifecycles[entry.Runner] = &runnerLifecycle{jobs: make(map[string]struct{})}
		case entry.EventType == "scale_up" && entry.Action == "requested":
			if !lastScaleDownAt.IsZero() && !entry.Time.Before(lastScaleDownAt) {
				scaleGaps = append(scaleGaps, entry.Time.Sub(lastScaleDownAt).Seconds())
				lastScaleDownAt = time.Time{}
			}
		case entry.EventType == "scale_down" && entry.Action == "started" && entry.Runner != "":
			if lifecycle := activeLifecycles[entry.Runner]; lifecycle != nil {
				lifecycleJobCounts = append(lifecycleJobCounts, float64(len(lifecycle.jobs)))
				delete(activeLifecycles, entry.Runner)
			}
			lastScaleDownAt = entry.Time
		}
	}

	metrics.QueueWaitSamples = len(queueWaits)
	metrics.AvgQueueWaitS = average(queueWaits)
	metrics.P95QueueWaitS = percentile(queueWaits, 0.95)
	metrics.LifecycleSamples = len(lifecycleJobCounts)
	metrics.AvgJobsPerLifecycle = average(lifecycleJobCounts)
	metrics.ReusedLifecyclePct = reusedLifecyclePct(lifecycleJobCounts)
	metrics.ScaleDownToScaleUpSamples = len(scaleGaps)
	metrics.AvgScaleDownToScaleUpS = average(scaleGaps)
	return metrics
}

func analyticsJobKey(entry domain.LogEntry) string {
	if entry.JobID != 0 {
		return strconv.FormatInt(entry.JobID, 10)
	}
	return fmt.Sprintf("%s|%d|%d|%s|%s", entry.Repo, entry.RunID, entry.RunAttempt, entry.Workflow, entry.Job)
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func percentile(values []float64, target float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if target <= 0 {
		return sorted[0]
	}
	if target >= 1 {
		return sorted[len(sorted)-1]
	}
	index := int(target*float64(len(sorted)-1) + 0.999999999)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func reusedLifecyclePct(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	reused := 0
	for _, value := range values {
		if value > 1 {
			reused++
		}
	}
	return float64(reused) / float64(len(values)) * 100
}
