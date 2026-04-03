// Package github implements iface.CIProvider via the GitHub Actions API.
package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v74/github"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// Provider implements CIProvider for GitHub Actions.
type Provider struct {
	client    *gh.Client
	org       string
	prefix    string
	validator *WebhookValidator
}

// New creates a GitHub CI provider.
func New(token, org, prefix string) *Provider {
	client := gh.NewClient(nil).WithAuthToken(token)
	return &Provider{
		client: client,
		org:    org,
		prefix: prefix,
	}
}

// ListRunners returns all runners registered with the org.
func (p *Provider) ListRunners(ctx context.Context) ([]domain.Runner, error) {
	opts := &gh.ListRunnersOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	result, _, err := p.client.Actions.ListOrganizationRunners(ctx, p.org, opts)
	if err != nil {
		return nil, fmt.Errorf("listing runners: %w", err)
	}

	runners := make([]domain.Runner, 0, len(result.Runners))
	for _, r := range result.Runners {
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.GetName())
		}
		runners = append(runners, domain.Runner{
			ID:     r.GetID(),
			Name:   r.GetName(),
			Status: r.GetStatus(),
			Busy:   r.GetBusy(),
			Labels: labels,
			IsAuto: strings.HasPrefix(r.GetName(), p.prefix),
		})
	}
	return runners, nil
}

// GetRegistrationToken returns a short-lived runner registration token.
func (p *Provider) GetRegistrationToken(ctx context.Context) (string, error) {
	token, _, err := p.client.Actions.CreateOrganizationRegistrationToken(ctx, p.org)
	if err != nil {
		return "", fmt.Errorf("creating registration token: %w", err)
	}
	return token.GetToken(), nil
}

// GetRemoveToken returns a short-lived runner removal token.
func (p *Provider) GetRemoveToken(ctx context.Context) (string, error) {
	token, _, err := p.client.Actions.CreateOrganizationRemoveToken(ctx, p.org)
	if err != nil {
		return "", fmt.Errorf("creating remove token: %w", err)
	}
	return token.GetToken(), nil
}

// DeleteRunner removes a runner by ID from the org.
func (p *Provider) DeleteRunner(ctx context.Context, runnerID int64) error {
	_, err := p.client.Actions.RemoveOrganizationRunner(ctx, p.org, runnerID)
	if err != nil {
		return fmt.Errorf("deleting runner %d: %w", runnerID, err)
	}
	return nil
}

// RegistrationURL returns the org URL for runner config.sh --url.
func (p *Provider) RegistrationURL() string {
	return "https://github.com/" + p.org
}

// ClassifyRunner returns true if the runner name matches the auto-scaled prefix.
func (p *Provider) ClassifyRunner(name string) bool {
	return strings.HasPrefix(name, p.prefix)
}

// ListRecentWorkflowRuns returns completed workflow runs across all org repos.
func (p *Provider) ListRecentWorkflowRuns(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error) {
	repos, _, err := p.client.Repositories.ListByOrg(ctx, p.org, &gh.RepositoryListByOrgOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("listing org repos: %w", err)
	}

	var results []domain.WorkflowMetrics
	for _, repo := range repos {
		runs, _, err := p.client.Actions.ListRepositoryWorkflowRuns(ctx, p.org, repo.GetName(), &gh.ListWorkflowRunsOptions{
			Status:      "completed",
			ListOptions: gh.ListOptions{PerPage: perRepo},
		})
		if err != nil {
			continue
		}

		for _, run := range runs.WorkflowRuns {
			durationS := 0
			created := run.GetCreatedAt()
			updated := run.GetUpdatedAt()
			if !created.IsZero() && !updated.IsZero() {
				durationS = int(updated.Time.Sub(created.Time).Seconds())
			}

			results = append(results, domain.WorkflowMetrics{
				Repo:       repo.GetName(),
				Workflow:   run.GetName(),
				Conclusion: run.GetConclusion(),
				DurationS:  durationS,
				RunNumber:  run.GetRunNumber(),
				Event:      run.GetEvent(),
				Branch:     run.GetHeadBranch(),
			})
		}
	}
	return results, nil
}
