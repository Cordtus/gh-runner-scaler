package github

import (
	"context"
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

func (p *Provider) hydrateWorkflowFailureDetails(ctx context.Context, repo string, run *gh.WorkflowRun) (string, string, string) {
	if run == nil {
		return "", "", ""
	}

	jobs, err := p.listWorkflowJobsForRun(ctx, repo, run.GetID(), run.GetRunAttempt())
	if err != nil || len(jobs) == 0 {
		return "", "", run.GetConclusion()
	}

	sort.SliceStable(jobs, func(i, j int) bool {
		return workflowJobOrderTime(jobs[i]).Before(workflowJobOrderTime(jobs[j]))
	})

	for _, job := range jobs {
		if !shouldEnrichWorkflowFailure(job.GetConclusion()) {
			continue
		}

		failedStep := firstFailedStep(job)
		reason := run.GetConclusion()
		if failedStep != "" {
			reason = failedStep
		} else if conclusion := job.GetConclusion(); conclusion != "" {
			reason = conclusion
		}
		return job.GetName(), failedStep, reason
	}

	return "", "", run.GetConclusion()
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
