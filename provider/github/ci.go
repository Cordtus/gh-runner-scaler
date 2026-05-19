// Package github implements iface.CIProvider via the GitHub Actions API.
package github

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v74/github"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const (
	defaultRunnerCacheTTL = 30 * time.Second
	defaultRunnerStaleTTL = 10 * time.Minute
)

// Provider implements CIProvider for GitHub Actions.
type Provider struct {
	client                *gh.Client
	org                   string
	prefix                string
	validator             *WebhookValidator
	mu                    sync.Mutex
	runnerFetchMu         sync.Mutex
	runnerCacheTTL        time.Duration
	runnerStaleTTL        time.Duration
	runnerCache           []domain.Runner
	runnerCacheFetchedAt  time.Time
	workflowRepoBatchSize int
	repoCacheTTL          time.Duration
	repoCache             []string
	repoCacheExpiresAt    time.Time
	workflowRepoCursor    int
}

// New creates a GitHub CI provider.
func New(token, org, prefix string) *Provider {
	client := gh.NewClient(nil).WithAuthToken(token)
	return newProvider(client, org, prefix)
}

func newProvider(client *gh.Client, org, prefix string) *Provider {
	return &Provider{
		client:                client,
		org:                   org,
		prefix:                prefix,
		runnerCacheTTL:        defaultRunnerCacheTTL,
		runnerStaleTTL:        defaultRunnerStaleTTL,
		workflowRepoBatchSize: 25,
		repoCacheTTL:          10 * time.Minute,
	}
}

// SetRunnerCacheTTL bounds how long fresh runner inventory can be reused.
func (p *Provider) SetRunnerCacheTTL(ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runnerCacheTTL = ttl
}

// SetRunnerStaleTTL bounds how long metrics may reuse stale runner inventory after API failures.
func (p *Provider) SetRunnerStaleTTL(ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runnerStaleTTL = ttl
}

// SetWorkflowRepoBatchSize bounds workflow metrics collection to a repo subset per interval.
// Zero disables the cap and scans every repo in the org.
func (p *Provider) SetWorkflowRepoBatchSize(size int) {
	if size < 0 {
		size = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workflowRepoBatchSize = size
}

// ListRunners returns all runners registered with the org.
func (p *Provider) ListRunners(ctx context.Context) ([]domain.Runner, error) {
	p.runnerFetchMu.Lock()
	defer p.runnerFetchMu.Unlock()

	runners, err := p.fetchRunners(ctx)
	if err != nil {
		return nil, err
	}
	p.storeRunnerCache(runners)
	return append([]domain.Runner(nil), runners...), nil
}

// ListRunnersForMetrics returns runner inventory with bounded stale fallback for dashboards.
func (p *Provider) ListRunnersForMetrics(ctx context.Context) ([]domain.Runner, domain.RunnerInventoryMeta, error) {
	return p.listRunners(ctx, true)
}

func (p *Provider) listRunners(ctx context.Context, allowStale bool) ([]domain.Runner, domain.RunnerInventoryMeta, error) {
	if runners, meta, ok := p.cachedRunners(false); ok {
		return runners, meta, nil
	}

	p.runnerFetchMu.Lock()
	defer p.runnerFetchMu.Unlock()

	if runners, meta, ok := p.cachedRunners(false); ok {
		return runners, meta, nil
	}

	runners, err := p.fetchRunners(ctx)
	if err != nil {
		if allowStale {
			if cached, meta, ok := p.cachedRunners(true); ok {
				meta.Stale = true
				meta.Error = err.Error()
				return cached, meta, nil
			}
		}
		return nil, domain.RunnerInventoryMeta{}, err
	}

	p.storeRunnerCache(runners)
	return append([]domain.Runner(nil), runners...), p.runnerInventoryMeta(time.Now(), false), nil
}

func (p *Provider) fetchRunners(ctx context.Context) ([]domain.Runner, error) {
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

func (p *Provider) storeRunnerCache(runners []domain.Runner) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.runnerCache = append([]domain.Runner(nil), runners...)
	p.runnerCacheFetchedAt = time.Now()
}

func (p *Provider) cachedRunners(allowStale bool) ([]domain.Runner, domain.RunnerInventoryMeta, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runnerCacheFetchedAt.IsZero() {
		return nil, domain.RunnerInventoryMeta{}, false
	}

	now := time.Now()
	age := now.Sub(p.runnerCacheFetchedAt)
	if age < 0 {
		age = 0
	}
	if p.runnerCacheTTL > 0 && age <= p.runnerCacheTTL {
		return append([]domain.Runner(nil), p.runnerCache...), p.runnerInventoryMetaLocked(now, false), true
	}
	if allowStale && p.runnerStaleTTL > 0 && age <= p.runnerStaleTTL {
		return append([]domain.Runner(nil), p.runnerCache...), p.runnerInventoryMetaLocked(now, true), true
	}
	return nil, domain.RunnerInventoryMeta{}, false
}

func (p *Provider) runnerInventoryMeta(now time.Time, stale bool) domain.RunnerInventoryMeta {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runnerInventoryMetaLocked(now, stale)
}

func (p *Provider) runnerInventoryMetaLocked(now time.Time, stale bool) domain.RunnerInventoryMeta {
	age := now.Sub(p.runnerCacheFetchedAt)
	if age < 0 {
		age = 0
	}
	ageSeconds := int(math.Round(age.Seconds()))
	return domain.RunnerInventoryMeta{
		Stale:     stale,
		AgeS:      ageSeconds,
		FetchedAt: p.runnerCacheFetchedAt.UTC().Format(time.RFC3339),
	}
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
	runs, err := p.ListRecentWorkflowRunsShallow(ctx, perRepo)
	if err != nil || len(runs) == 0 {
		return runs, err
	}

	enriched, _ := p.EnrichWorkflowMetrics(ctx, runs)
	if len(enriched) > 0 {
		runs = enriched
	}
	return runs, nil
}

// ListRecentWorkflowRunsShallow returns completed workflow runs without failure-job enrichment.
func (p *Provider) ListRecentWorkflowRunsShallow(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error) {
	if perRepo <= 0 {
		return nil, nil
	}

	repos, err := p.listOrgRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing org repos: %w", err)
	}
	repos = p.workflowRepoBatch(repos)

	var results []domain.WorkflowMetrics
	for _, repo := range repos {
		runs, err := p.listRepositoryWorkflowRuns(ctx, repo, perRepo)
		if err != nil {
			continue
		}

		for _, run := range runs {
			durationS := 0
			created := run.GetCreatedAt()
			started := run.GetRunStartedAt()
			updated := run.GetUpdatedAt()
			if !started.IsZero() && !updated.IsZero() {
				durationS = int(updated.Time.Sub(started.Time).Seconds())
			} else if !created.IsZero() && !updated.IsZero() {
				durationS = int(updated.Time.Sub(created.Time).Seconds())
			}
			if durationS < 0 {
				durationS = 0
			}
			completedAt := ""
			if !updated.IsZero() {
				completedAt = updated.Time.UTC().Format(time.RFC3339)
			} else if !created.IsZero() {
				completedAt = created.Time.UTC().Format(time.RFC3339)
			}

			results = append(results, domain.WorkflowMetrics{
				RunID:       run.GetID(),
				RunAttempt:  run.GetRunAttempt(),
				Repo:        repo,
				Workflow:    run.GetName(),
				Conclusion:  run.GetConclusion(),
				DurationS:   durationS,
				RunNumber:   run.GetRunNumber(),
				Event:       run.GetEvent(),
				Branch:      run.GetHeadBranch(),
				CompletedAt: completedAt,
			})
		}
	}
	return results, nil
}

// EnrichWorkflowMetrics hydrates failure details only for fresh failed workflow runs.
func (p *Provider) EnrichWorkflowMetrics(ctx context.Context, runs []domain.WorkflowMetrics) ([]domain.WorkflowMetrics, error) {
	if len(runs) == 0 {
		return nil, nil
	}

	enriched := append([]domain.WorkflowMetrics(nil), runs...)
	var firstErr error
	for i, run := range enriched {
		if !shouldEnrichWorkflowFailure(run.Conclusion) {
			continue
		}

		failedJob, failedStep, failureReason, err := p.hydrateWorkflowFailureDetails(
			ctx,
			run.Repo,
			run.RunID,
			run.RunAttempt,
			run.Conclusion,
		)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		enriched[i].FailedJob = failedJob
		enriched[i].FailedStep = failedStep
		enriched[i].FailureReason = failureReason
	}

	return enriched, firstErr
}

func (p *Provider) listOrgRepos(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	if len(p.repoCache) > 0 && time.Now().Before(p.repoCacheExpiresAt) {
		cached := append([]string(nil), p.repoCache...)
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	opts := &gh.RepositoryListByOrgOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var repos []string
	for {
		pageRepos, resp, err := p.client.Repositories.ListByOrg(ctx, p.org, opts)
		if err != nil {
			return nil, err
		}
		for _, repo := range pageRepos {
			repos = append(repos, repo.GetName())
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	p.mu.Lock()
	p.repoCache = append([]string(nil), repos...)
	p.repoCacheExpiresAt = time.Now().Add(p.repoCacheTTL)
	if len(repos) == 0 {
		p.workflowRepoCursor = 0
	} else {
		p.workflowRepoCursor %= len(repos)
	}
	p.mu.Unlock()

	return repos, nil
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

func (p *Provider) workflowRepoBatch(repos []string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	limit := p.workflowRepoBatchSize
	if limit == 0 || len(repos) <= limit {
		return append([]string(nil), repos...)
	}

	start := p.workflowRepoCursor % len(repos)
	batch := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		batch = append(batch, repos[(start+i)%len(repos)])
	}
	p.workflowRepoCursor = (start + limit) % len(repos)
	return batch
}
