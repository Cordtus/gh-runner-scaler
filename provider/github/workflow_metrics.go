package github

import (
	"context"
	"fmt"
	"sort"
	"time"

	gh "github.com/google/go-github/v74/github"
)

func shouldEnrichWorkflowFailure(conclusion string) bool {
	switch conclusion {
	case "", "success", "neutral", "skipped":
		return false
	default:
		return true
	}
}

func (p *Provider) hydrateWorkflowFailureDetails(ctx context.Context, repo string, runID int64, attempt int, conclusion string) (string, string, string, error) {
	jobs, err := p.listWorkflowJobsForRun(ctx, repo, runID, attempt)
	if err != nil {
		return "", "", conclusion, fmt.Errorf("listing workflow jobs for %s run %d attempt %d: %w", repo, runID, attempt, err)
	}
	if len(jobs) == 0 {
		return "", "", conclusion, nil
	}

	sort.SliceStable(jobs, func(i, j int) bool {
		return workflowJobOrderTime(jobs[i]).Before(workflowJobOrderTime(jobs[j]))
	})

	for _, job := range jobs {
		if !shouldEnrichWorkflowFailure(job.GetConclusion()) {
			continue
		}

		failedStep := firstFailedStep(job)
		reason := conclusion
		if failedStep != "" {
			reason = failedStep
		} else if conclusion := job.GetConclusion(); conclusion != "" {
			reason = conclusion
		}
		return job.GetName(), failedStep, reason, nil
	}

	return "", "", conclusion, nil
}

func (p *Provider) listWorkflowJobsForRun(ctx context.Context, repo string, runID int64, attempt int) ([]*gh.WorkflowJob, error) {
	if runID == 0 {
		return nil, nil
	}

	var jobs []*gh.WorkflowJob
	if attempt > 0 {
		opts := &gh.ListOptions{PerPage: 100}
		for {
			result, resp, err := p.client.Actions.ListWorkflowJobsAttempt(ctx, p.org, repo, runID, int64(attempt), opts)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, result.Jobs...)
			if resp.NextPage == 0 {
				return jobs, nil
			}
			opts.Page = resp.NextPage
		}
	}

	opts := &gh.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for {
		result, resp, err := p.client.Actions.ListWorkflowJobs(ctx, p.org, repo, runID, opts)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, result.Jobs...)
		if resp.NextPage == 0 {
			return jobs, nil
		}
		opts.Page = resp.NextPage
	}
}

func firstFailedStep(job *gh.WorkflowJob) string {
	if job == nil {
		return ""
	}
	for _, step := range job.Steps {
		if step == nil {
			continue
		}
		if shouldEnrichWorkflowFailure(step.GetConclusion()) {
			return step.GetName()
		}
	}
	return ""
}

func workflowJobOrderTime(job *gh.WorkflowJob) time.Time {
	if job == nil {
		return time.Time{}
	}
	if started := job.GetStartedAt(); !started.IsZero() {
		return started.Time
	}
	if created := job.GetCreatedAt(); !created.IsZero() {
		return created.Time
	}
	if completed := job.GetCompletedAt(); !completed.IsZero() {
		return completed.Time
	}
	return time.Time{}
}
