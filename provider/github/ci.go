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
	return newProvider(client, org, prefix)
}

func newProvider(client *gh.Client, org, prefix string) *Provider {
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
	var runners []domain.Runner
	for {
		result, resp, err := p.client.Actions.ListOrganizationRunners(ctx, p.org, opts)
		if err != nil {
			return nil, fmt.Errorf("listing runners: %w", err)
		}

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

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
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
	if perRepo <= 0 {
		return nil, nil
	}

	repos, err := p.listOrgRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing org repos: %w", err)
	}

	var results []domain.WorkflowMetrics
	for _, repo := range repos {
		runs, err := p.listRepositoryWorkflowRuns(ctx, repo.GetName(), perRepo)
		if err != nil {
			continue
		}

		for _, run := range runs {
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

func (p *Provider) listOrgRepos(ctx context.Context) ([]*gh.Repository, error) {
	opts := &gh.RepositoryListByOrgOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var repos []*gh.Repository
	for {
		pageRepos, resp, err := p.client.Repositories.ListByOrg(ctx, p.org, opts)
		if err != nil {
			return nil, err
		}
		repos = append(repos, pageRepos...)
		if resp.NextPage == 0 {
			return repos, nil
		}
		opts.Page = resp.NextPage
	}
}

func (p *Provider) listRepositoryWorkflowRuns(ctx context.Context, repo string, limit int) ([]*gh.WorkflowRun, error) {
	perPage := limit
	if perPage > 100 {
		perPage = 100
	}
	opts := &gh.ListWorkflowRunsOptions{
		Status:      "completed",
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	var runs []*gh.WorkflowRun
	for {
		result, resp, err := p.client.Actions.ListRepositoryWorkflowRuns(ctx, p.org, repo, opts)
		if err != nil {
			return nil, err
		}
		for _, run := range result.WorkflowRuns {
			runs = append(runs, run)
			if len(runs) == limit {
				return runs, nil
			}
		}
		if resp.NextPage == 0 {
			return runs, nil
		}
		opts.Page = resp.NextPage
	}
}
