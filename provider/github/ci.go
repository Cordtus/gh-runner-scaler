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
	defaultRunnerCacheTTL      = 5 * time.Second
	defaultRunnerStaleTTL      = 10 * time.Minute
	defaultTokenRefreshPadding = 5 * time.Minute
)

// Provider implements CIProvider for GitHub Actions.
type Provider struct {
	client                *gh.Client
	org                   string
	prefix                string
	validator             *WebhookValidator
	mu                    sync.Mutex
	runnerCacheTTL        time.Duration
	runnerStaleTTL        time.Duration
	runnerCache           *runnerInventoryCache
	workflowRepoBatchSize int
	repoCacheTTL          time.Duration
	repoCache             []string
	repoCacheExpiresAt    time.Time
	workflowRepoCursor    int
	registrationToken     cachedToken
	registrationFetchMu   sync.Mutex
	removeToken           cachedToken
	removeFetchMu         sync.Mutex
}

type runnerInventoryCache struct {
	mu         sync.Mutex
	fetchMu    sync.Mutex
	runners    []domain.Runner
	fetchedAt  time.Time
	suspended  bool
	generation uint64
}

type cachedToken struct {
	value     string
	expiresAt time.Time
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
		runnerCache:           &runnerInventoryCache{},
		workflowRepoBatchSize: 25,
		repoCacheTTL:          10 * time.Minute,
	}
}

// ShareRunnerCacheWith lets providers for the same org reuse a recent runner inventory.
func (p *Provider) ShareRunnerCacheWith(other *Provider) {
	if other == nil || other == p {
		return
	}
	otherCache := other.currentRunnerCache()
	p.mu.Lock()
	p.runnerCache = otherCache
	p.mu.Unlock()
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
	runners, _, err := p.listRunners(ctx, false)
	return runners, err
}

// ListRunnersForMetrics returns runner inventory with bounded stale fallback for dashboards.
func (p *Provider) ListRunnersForMetrics(ctx context.Context) ([]domain.Runner, domain.RunnerInventoryMeta, error) {
	return p.listRunners(ctx, true)
}

func (p *Provider) listRunners(ctx context.Context, allowStale bool) ([]domain.Runner, domain.RunnerInventoryMeta, error) {
	if runners, meta, ok := p.cachedRunners(false); ok {
		return runners, meta, nil
	}

	cache := p.currentRunnerCache()
	cache.fetchMu.Lock()
	defer cache.fetchMu.Unlock()

	if runners, meta, ok := p.cachedRunners(false); ok {
		return runners, meta, nil
	}
	generation := p.runnerCacheGeneration()

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

	fetchedAt := time.Now()
	p.storeRunnerCache(runners, generation, fetchedAt)
	return p.annotateRunners(runners), runnerInventoryMeta(fetchedAt, time.Now(), false), nil
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
			})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return runners, nil
}

func (p *Provider) storeRunnerCache(runners []domain.Runner, generation uint64, fetchedAt time.Time) {
	cache := p.currentRunnerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cache.generation != generation {
		return
	}
	cache.runners = append([]domain.Runner(nil), runners...)
	cache.fetchedAt = fetchedAt
}

func (p *Provider) cachedRunners(allowStale bool) ([]domain.Runner, domain.RunnerInventoryMeta, bool) {
	cache := p.currentRunnerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cache.fetchedAt.IsZero() {
		return nil, domain.RunnerInventoryMeta{}, false
	}

	now := time.Now()
	age := now.Sub(cache.fetchedAt)
	if age < 0 {
		age = 0
	}
	runnerCacheTTL, runnerStaleTTL := p.runnerCacheTTLs()
	if !allowStale && cache.suspended {
		return nil, domain.RunnerInventoryMeta{}, false
	}
	if runnerCacheTTL > 0 && age <= runnerCacheTTL {
		return p.annotateRunners(cache.runners), runnerInventoryMeta(cache.fetchedAt, now, false), true
	}
	if allowStale && runnerStaleTTL > 0 && age <= runnerStaleTTL {
		return p.annotateRunners(cache.runners), runnerInventoryMeta(cache.fetchedAt, now, true), true
	}
	return nil, domain.RunnerInventoryMeta{}, false
}

func (p *Provider) runnerCacheGeneration() uint64 {
	cache := p.currentRunnerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.generation
}

func runnerInventoryMeta(fetchedAt, now time.Time, stale bool) domain.RunnerInventoryMeta {
	age := now.Sub(fetchedAt)
	if age < 0 {
		age = 0
	}
	ageSeconds := int(math.Round(age.Seconds()))
	return domain.RunnerInventoryMeta{
		Stale:     stale,
		AgeS:      ageSeconds,
		FetchedAt: fetchedAt.UTC().Format(time.RFC3339),
	}
}

// GetRegistrationToken returns a short-lived runner registration token.
func (p *Provider) GetRegistrationToken(ctx context.Context) (string, error) {
	if token, ok := p.cachedRegistrationToken(); ok {
		p.SuspendRunnerInventoryCache()
		return token, nil
	}

	p.registrationFetchMu.Lock()
	defer p.registrationFetchMu.Unlock()

	if token, ok := p.cachedRegistrationToken(); ok {
		p.SuspendRunnerInventoryCache()
		return token, nil
	}

	token, _, err := p.client.Actions.CreateOrganizationRegistrationToken(ctx, p.org)
	if err != nil {
		return "", fmt.Errorf("creating registration token: %w", err)
	}
	value := token.GetToken()
	p.storeRegistrationToken(value, tokenExpiresAt(token.ExpiresAt))
	p.SuspendRunnerInventoryCache()
	return value, nil
}

// GetRemoveToken returns a short-lived runner removal token.
func (p *Provider) GetRemoveToken(ctx context.Context) (string, error) {
	if token, ok := p.cachedRemoveToken(); ok {
		p.SuspendRunnerInventoryCache()
		return token, nil
	}

	p.removeFetchMu.Lock()
	defer p.removeFetchMu.Unlock()

	if token, ok := p.cachedRemoveToken(); ok {
		p.SuspendRunnerInventoryCache()
		return token, nil
	}

	token, _, err := p.client.Actions.CreateOrganizationRemoveToken(ctx, p.org)
	if err != nil {
		return "", fmt.Errorf("creating remove token: %w", err)
	}
	value := token.GetToken()
	p.storeRemoveToken(value, tokenExpiresAt(token.ExpiresAt))
	p.SuspendRunnerInventoryCache()
	return value, nil
}

// DeleteRunner removes a runner by ID from the org.
func (p *Provider) DeleteRunner(ctx context.Context, runnerID int64) error {
	_, err := p.client.Actions.RemoveOrganizationRunner(ctx, p.org, runnerID)
	if err != nil {
		return fmt.Errorf("deleting runner %d: %w", runnerID, err)
	}
	p.invalidateRunnerCache()
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

func (p *Provider) currentRunnerCache() *runnerInventoryCache {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runnerCache == nil {
		p.runnerCache = &runnerInventoryCache{}
	}
	return p.runnerCache
}

func (p *Provider) runnerCacheTTLs() (time.Duration, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runnerCacheTTL, p.runnerStaleTTL
}

func (p *Provider) invalidateRunnerCache() {
	cache := p.currentRunnerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.runners = nil
	cache.fetchedAt = time.Time{}
	cache.generation++
}

// SuspendRunnerInventoryCache prevents reconcile from using snapshots fetched during runner mutations.
func (p *Provider) SuspendRunnerInventoryCache() {
	cache := p.currentRunnerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.runners = nil
	cache.fetchedAt = time.Time{}
	cache.suspended = true
	cache.generation++
}

// ResumeRunnerInventoryCache allows reconcile cache hits after clearing mutation-period snapshots.
func (p *Provider) ResumeRunnerInventoryCache() {
	cache := p.currentRunnerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.runners = nil
	cache.fetchedAt = time.Time{}
	cache.suspended = false
	cache.generation++
}

func (p *Provider) annotateRunners(runners []domain.Runner) []domain.Runner {
	result := append([]domain.Runner(nil), runners...)
	for i := range result {
		result[i].IsAuto = strings.HasPrefix(result[i].Name, p.prefix)
	}
	return result
}

func (p *Provider) cachedRegistrationToken() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return usableCachedToken(p.registrationToken)
}

func (p *Provider) storeRegistrationToken(value string, expiresAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registrationToken = cachedToken{value: value, expiresAt: expiresAt}
}

func (p *Provider) cachedRemoveToken() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return usableCachedToken(p.removeToken)
}

func (p *Provider) storeRemoveToken(value string, expiresAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeToken = cachedToken{value: value, expiresAt: expiresAt}
}

func usableCachedToken(token cachedToken) (string, bool) {
	if token.value == "" || token.expiresAt.IsZero() {
		return "", false
	}
	return token.value, time.Now().Add(defaultTokenRefreshPadding).Before(token.expiresAt)
}

func tokenExpiresAt(ts *gh.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.Time
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
