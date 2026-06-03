package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v74/github"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestListRunners_PaginatesAcrossAllPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners" {
			http.NotFound(w, r)
			return
		}

		payload := map[string]any{
			"total_count": 2,
			"runners": []map[string]any{{
				"id":     1,
				"name":   "auto-1",
				"status": "online",
				"busy":   false,
				"labels": []map[string]any{},
			}},
		}
		if r.URL.Query().Get("page") == "2" {
			payload["runners"] = []map[string]any{{
				"id":     2,
				"name":   "auto-2",
				"status": "online",
				"busy":   true,
				"labels": []map[string]any{},
			}}
		} else {
			w.Header().Set("Link", "<"+server.URL+"/orgs/test-org/actions/runners?page=2>; rel=\"next\"")
		}

		writeJSON(t, w, payload)
	}))
	defer server.Close()

	provider := testProvider(t, server)
	runners, err := provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners returned error: %v", err)
	}
	if len(runners) != 2 {
		t.Fatalf("expected 2 runners, got %d", len(runners))
	}
	if runners[1].Name != "auto-2" {
		t.Fatalf("expected second runner from next page, got %q", runners[1].Name)
	}
}

func TestListRunners_ReusesFreshCache(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners" {
			http.NotFound(w, r)
			return
		}
		calls++
		writeJSON(t, w, map[string]any{
			"total_count": 1,
			"runners": []map[string]any{{
				"id":     1,
				"name":   "auto-1",
				"status": "online",
				"busy":   false,
				"labels": []map[string]any{},
			}},
		})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	provider.SetRunnerCacheTTL(time.Minute)
	first, err := provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("first ListRunners returned error: %v", err)
	}
	second, err := provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("second ListRunners returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner API calls = %d, want 1", calls)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Name != second[0].Name {
		t.Fatalf("unexpected runners: first=%+v second=%+v", first, second)
	}
}

func TestListRunners_SharedCacheAnnotatesPerProviderPrefix(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners" {
			http.NotFound(w, r)
			return
		}
		calls++
		writeJSON(t, w, map[string]any{
			"total_count": 2,
			"runners": []map[string]any{
				{
					"id":     1,
					"name":   "auto-a-1",
					"status": "online",
					"busy":   false,
					"labels": []map[string]any{},
				},
				{
					"id":     2,
					"name":   "auto-b-1",
					"status": "online",
					"busy":   false,
					"labels": []map[string]any{},
				},
			},
		})
	}))
	defer server.Close()

	firstProvider := testProviderWithPrefix(t, server, "auto-a")
	secondProvider := testProviderWithPrefix(t, server, "auto-b")
	firstProvider.SetRunnerCacheTTL(time.Minute)
	secondProvider.SetRunnerCacheTTL(time.Minute)
	secondProvider.ShareRunnerCacheWith(firstProvider)

	first, err := firstProvider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("first ListRunners returned error: %v", err)
	}
	second, err := secondProvider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("second ListRunners returned error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("runner API calls = %d, want 1", calls)
	}
	if !first[0].IsAuto || first[1].IsAuto {
		t.Fatalf("first provider auto flags = [%v %v], want [true false]", first[0].IsAuto, first[1].IsAuto)
	}
	if second[0].IsAuto || !second[1].IsAuto {
		t.Fatalf("second provider auto flags = [%v %v], want [false true]", second[0].IsAuto, second[1].IsAuto)
	}
}

func TestListRunnersForMetrics_ReusesFreshEmptyCache(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners" {
			http.NotFound(w, r)
			return
		}
		calls++
		writeJSON(t, w, map[string]any{
			"total_count": 0,
			"runners":     []map[string]any{},
		})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	provider.SetRunnerCacheTTL(time.Minute)
	first, err := provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("first ListRunners returned error: %v", err)
	}
	second, meta, err := provider.ListRunnersForMetrics(context.Background())
	if err != nil {
		t.Fatalf("ListRunnersForMetrics returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner API calls = %d, want 1", calls)
	}
	if meta.Stale {
		t.Fatal("meta.Stale = true, want false")
	}
	if len(first) != 0 || len(second) != 0 {
		t.Fatalf("unexpected cached runners: first=%+v second=%+v", first, second)
	}
}

func TestListRunnersForMetrics_UsesStaleCacheOnFailure(t *testing.T) {
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners" {
			http.NotFound(w, r)
			return
		}
		if fail {
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		writeJSON(t, w, map[string]any{
			"total_count": 1,
			"runners": []map[string]any{{
				"id":     1,
				"name":   "auto-1",
				"status": "online",
				"busy":   false,
				"labels": []map[string]any{},
			}},
		})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	provider.SetRunnerCacheTTL(1 * time.Nanosecond)
	provider.SetRunnerStaleTTL(time.Hour)
	if _, err := provider.ListRunners(context.Background()); err != nil {
		t.Fatalf("priming ListRunners returned error: %v", err)
	}
	time.Sleep(time.Millisecond)
	fail = true

	runners, meta, err := provider.ListRunnersForMetrics(context.Background())
	if err != nil {
		t.Fatalf("ListRunnersForMetrics returned error: %v", err)
	}
	if !meta.Stale {
		t.Fatal("meta.Stale = false, want true")
	}
	if meta.AgeS < 0 {
		t.Fatalf("meta.AgeS = %d, want >= 0", meta.AgeS)
	}
	if meta.FetchedAt == "" {
		t.Fatal("meta.FetchedAt is empty")
	}
	if !strings.Contains(meta.Error, "listing runners") || !strings.Contains(meta.Error, "403") {
		t.Fatalf("meta.Error = %q, want wrapped 403 error", meta.Error)
	}
	if len(runners) != 1 || runners[0].Name != "auto-1" {
		t.Fatalf("unexpected stale runners: %+v", runners)
	}

	if _, err := provider.ListRunners(context.Background()); err == nil {
		t.Fatal("ListRunners returned nil error after cache expiry, want API failure")
	}
}

func TestRunnerTokens_AreCachedUntilNearExpiry(t *testing.T) {
	var registrationCalls int
	var removeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/test-org/actions/runners/registration-token":
			registrationCalls++
			writeJSON(t, w, map[string]any{
				"token":      "registration-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			})
		case "/orgs/test-org/actions/runners/remove-token":
			removeCalls++
			writeJSON(t, w, map[string]any{
				"token":      "remove-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	for i := 0; i < 2; i++ {
		registrationToken, err := provider.GetRegistrationToken(context.Background())
		if err != nil {
			t.Fatalf("GetRegistrationToken returned error: %v", err)
		}
		if registrationToken != "registration-token" {
			t.Fatalf("registration token = %q, want registration-token", registrationToken)
		}

		removeToken, err := provider.GetRemoveToken(context.Background())
		if err != nil {
			t.Fatalf("GetRemoveToken returned error: %v", err)
		}
		if removeToken != "remove-token" {
			t.Fatalf("remove token = %q, want remove-token", removeToken)
		}
	}

	if registrationCalls != 1 {
		t.Fatalf("registration token API calls = %d, want 1", registrationCalls)
	}
	if removeCalls != 1 {
		t.Fatalf("remove token API calls = %d, want 1", removeCalls)
	}
}

func TestRegistrationToken_InvalidatesRunnerCacheForReconcileAfterMetricsInterleaving(t *testing.T) {
	registered := false
	runnerCalls := 0
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/test-org/actions/runners":
			runnerCalls++
			runners := []map[string]any{}
			if registered {
				runners = append(runners, map[string]any{
					"id":     1,
					"name":   "auto-1",
					"status": "online",
					"busy":   false,
					"labels": []map[string]any{},
				})
			}
			writeJSON(t, w, map[string]any{
				"total_count": len(runners),
				"runners":     runners,
			})
		case "/orgs/test-org/actions/runners/registration-token":
			tokenCalls++
			writeJSON(t, w, map[string]any{
				"token":      "registration-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	provider.SetRunnerCacheTTL(time.Minute)

	runners, err := provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners returned error: %v", err)
	}
	if len(runners) != 0 {
		t.Fatalf("initial runners = %+v, want empty", runners)
	}
	if _, err := provider.GetRegistrationToken(context.Background()); err != nil {
		t.Fatalf("GetRegistrationToken returned error: %v", err)
	}
	metricsRunners, _, err := provider.ListRunnersForMetrics(context.Background())
	if err != nil {
		t.Fatalf("ListRunnersForMetrics during registration returned error: %v", err)
	}
	if len(metricsRunners) != 0 {
		t.Fatalf("metrics runners during registration = %+v, want empty", metricsRunners)
	}
	registered = true
	runners, err = provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners after token returned error: %v", err)
	}

	if runnerCalls != 3 {
		t.Fatalf("runner API calls = %d, want 3 after bypassing metrics-populated stale cache", runnerCalls)
	}
	if tokenCalls != 1 {
		t.Fatalf("token API calls = %d, want 1", tokenCalls)
	}
	if len(runners) != 1 || runners[0].Name != "auto-1" {
		t.Fatalf("runners after token = %+v, want auto-1", runners)
	}
}

func TestResumeRunnerInventoryCache_IgnoresInFlightMetricsFetchFromSuspendedGeneration(t *testing.T) {
	var mu sync.Mutex
	registered := false
	runnerCalls := 0
	metricsFetchStarted := make(chan struct{})
	releaseMetricsFetch := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners" {
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		runnerCalls++
		call := runnerCalls
		hasRegistered := registered
		mu.Unlock()

		if call == 1 {
			close(metricsFetchStarted)
			<-releaseMetricsFetch
		}

		runners := []map[string]any{}
		if hasRegistered {
			runners = append(runners, map[string]any{
				"id":     1,
				"name":   "auto-1",
				"status": "online",
				"busy":   false,
				"labels": []map[string]any{},
			})
		}
		writeJSON(t, w, map[string]any{
			"total_count": len(runners),
			"runners":     runners,
		})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	provider.SetRunnerCacheTTL(time.Minute)
	provider.SuspendRunnerInventoryCache()

	errCh := make(chan error, 1)
	go func() {
		runners, _, err := provider.ListRunnersForMetrics(context.Background())
		if err != nil {
			errCh <- err
			return
		}
		if len(runners) != 0 {
			errCh <- fmt.Errorf("metrics runners = %+v, want empty", runners)
			return
		}
		errCh <- nil
	}()

	<-metricsFetchStarted
	provider.ResumeRunnerInventoryCache()
	mu.Lock()
	registered = true
	mu.Unlock()
	close(releaseMetricsFetch)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	runners, err := provider.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners after resume returned error: %v", err)
	}
	if len(runners) != 1 || runners[0].Name != "auto-1" {
		t.Fatalf("runners after resume = %+v, want auto-1", runners)
	}

	mu.Lock()
	defer mu.Unlock()
	if runnerCalls != 2 {
		t.Fatalf("runner API calls = %d, want 2 because stale in-flight metrics fetch must not populate cache", runnerCalls)
	}
}

func TestRunnerTokens_RefreshNearExpiryAndMissingExpiry(t *testing.T) {
	tokenValues := []map[string]any{
		{
			"token":      "near-expiry",
			"expires_at": time.Now().Add(defaultTokenRefreshPadding - time.Second).UTC().Format(time.RFC3339Nano),
		},
		{
			"token": "missing-expiry",
		},
		{
			"token":      "fresh",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners/registration-token" {
			http.NotFound(w, r)
			return
		}
		if calls >= len(tokenValues) {
			t.Fatalf("unexpected extra token request %d", calls+1)
		}
		writeJSON(t, w, tokenValues[calls])
		calls++
	}))
	defer server.Close()

	provider := testProvider(t, server)
	want := []string{"near-expiry", "missing-expiry", "fresh", "fresh"}
	for _, expected := range want {
		token, err := provider.GetRegistrationToken(context.Background())
		if err != nil {
			t.Fatalf("GetRegistrationToken returned error: %v", err)
		}
		if token != expected {
			t.Fatalf("token = %q, want %q", token, expected)
		}
	}
	if calls != 3 {
		t.Fatalf("token API calls = %d, want 3", calls)
	}
}

func TestRunnerTokens_CoalesceConcurrentMisses(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/actions/runners/registration-token" {
			http.NotFound(w, r)
			return
		}
		calls++
		time.Sleep(10 * time.Millisecond)
		writeJSON(t, w, map[string]any{
			"token":      "registration-token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		})
	}))
	defer server.Close()

	provider := testProvider(t, server)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := provider.GetRegistrationToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if token != "registration-token" {
				errs <- fmt.Errorf("token = %q, want registration-token", token)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("registration token API calls = %d, want 1", calls)
	}
}

func TestListRecentWorkflowRuns_BatchesReposAndCachesRepoList(t *testing.T) {
	var repoListCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/test-org/repos":
			repoListCalls++
			repos := []map[string]any{{"name": "repo-a"}}
			if r.URL.Query().Get("page") == "2" {
				repos = []map[string]any{{"name": "repo-b"}}
			} else {
				w.Header().Set("Link", "<"+server.URL+"/orgs/test-org/repos?page=2>; rel=\"next\"")
			}
			writeJSON(t, w, repos)
		case strings.HasPrefix(r.URL.Path, "/repos/test-org/") && strings.HasSuffix(r.URL.Path, "/actions/runs"):
			repo := strings.TrimPrefix(r.URL.Path, "/repos/test-org/")
			repo = strings.TrimSuffix(repo, "/actions/runs")
			if repo != "repo-a" && repo != "repo-b" {
				t.Fatalf("unexpected repo in workflow run request: %s", repo)
			}
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"workflow_runs": []map[string]any{{
					"id":             101,
					"run_attempt":    2,
					"name":           "build",
					"conclusion":     "success",
					"run_number":     7,
					"event":          "push",
					"head_branch":    "main",
					"created_at":     "2026-04-19T12:00:00Z",
					"run_started_at": "2026-04-19T12:01:00Z",
					"updated_at":     "2026-04-19T12:01:30Z",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	provider.SetWorkflowRepoBatchSize(1)
	runs, err := provider.ListRecentWorkflowRuns(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListRecentWorkflowRuns returned error: %v", err)
	}
	if len(runs) != 1 || runs[0].Repo != "repo-a" {
		t.Fatalf("expected first batch to include repo-a, got %+v", runs)
	}
	if runs[0].RunID != 101 {
		t.Fatalf("expected run ID 101, got %d", runs[0].RunID)
	}
	if runs[0].RunAttempt != 2 {
		t.Fatalf("expected run attempt 2, got %d", runs[0].RunAttempt)
	}
	if runs[0].DurationS != 30 {
		t.Fatalf("expected duration to use run_started_at, got %d", runs[0].DurationS)
	}
	if runs[0].CompletedAt != "2026-04-19T12:01:30Z" {
		t.Fatalf("expected completed_at to match updated_at, got %q", runs[0].CompletedAt)
	}

	runs, err = provider.ListRecentWorkflowRuns(context.Background(), 1)
	if err != nil {
		t.Fatalf("second ListRecentWorkflowRuns returned error: %v", err)
	}
	if len(runs) != 1 || runs[0].Repo != "repo-b" {
		t.Fatalf("expected second batch to include repo-b, got %+v", runs)
	}
	if repoListCalls != 2 {
		t.Fatalf("expected one paginated repo fetch across two pages before caching, got %d calls", repoListCalls)
	}
}

func TestListRecentWorkflowRuns_EnrichesFailureDetailsFromWorkflowJobs(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/test-org/repos":
			writeJSON(t, w, []map[string]any{{"name": "repo-a"}})
		case r.URL.Path == "/repos/test-org/repo-a/actions/runs":
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"workflow_runs": []map[string]any{{
					"id":             101,
					"run_attempt":    2,
					"name":           "build",
					"conclusion":     "failure",
					"run_number":     7,
					"event":          "push",
					"head_branch":    "main",
					"created_at":     "2026-04-19T12:00:00Z",
					"run_started_at": "2026-04-19T12:01:00Z",
					"updated_at":     "2026-04-19T12:01:30Z",
				}},
			})
		case r.URL.Path == "/repos/test-org/repo-a/actions/runs/101/attempts/2/jobs":
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"jobs": []map[string]any{{
					"id":           501,
					"name":         "integration",
					"conclusion":   "failure",
					"started_at":   "2026-04-19T12:01:05Z",
					"completed_at": "2026-04-19T12:01:25Z",
					"steps": []map[string]any{{
						"name":       "Install",
						"conclusion": "success",
					}, {
						"name":       "Run tests",
						"conclusion": "failure",
					}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	runs, err := provider.ListRecentWorkflowRuns(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListRecentWorkflowRuns returned error: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].FailedJob != "integration" {
		t.Fatalf("FailedJob = %q, want integration", runs[0].FailedJob)
	}
	if runs[0].FailedStep != "Run tests" {
		t.Fatalf("FailedStep = %q, want Run tests", runs[0].FailedStep)
	}
	if runs[0].FailureReason != "Run tests" {
		t.Fatalf("FailureReason = %q, want Run tests", runs[0].FailureReason)
	}
}

func TestEnrichWorkflowMetrics_EnrichesFailureDetailsFromWorkflowJobs(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/test-org/repos":
			writeJSON(t, w, []map[string]any{{"name": "repo-a"}})
		case r.URL.Path == "/repos/test-org/repo-a/actions/runs":
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"workflow_runs": []map[string]any{{
					"id":             101,
					"run_attempt":    2,
					"name":           "build",
					"conclusion":     "failure",
					"run_number":     7,
					"event":          "push",
					"head_branch":    "main",
					"created_at":     "2026-04-19T12:00:00Z",
					"run_started_at": "2026-04-19T12:01:00Z",
					"updated_at":     "2026-04-19T12:01:30Z",
				}},
			})
		case r.URL.Path == "/repos/test-org/repo-a/actions/runs/101/attempts/2/jobs":
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"jobs": []map[string]any{{
					"id":           501,
					"name":         "integration",
					"conclusion":   "failure",
					"started_at":   "2026-04-19T12:01:05Z",
					"completed_at": "2026-04-19T12:01:25Z",
					"steps": []map[string]any{{
						"name":       "Install",
						"conclusion": "success",
					}, {
						"name":       "Run tests",
						"conclusion": "failure",
					}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	runs, err := provider.ListRecentWorkflowRunsShallow(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListRecentWorkflowRunsShallow returned error: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].FailedJob != "" || runs[0].FailedStep != "" || runs[0].FailureReason != "" {
		t.Fatalf("expected shallow run to be unenriched, got %+v", runs[0])
	}

	runs, err = provider.EnrichWorkflowMetrics(context.Background(), runs)
	if err != nil {
		t.Fatalf("EnrichWorkflowMetrics returned error: %v", err)
	}
	if runs[0].FailedJob != "integration" {
		t.Fatalf("FailedJob = %q, want integration", runs[0].FailedJob)
	}
	if runs[0].FailedStep != "Run tests" {
		t.Fatalf("FailedStep = %q, want Run tests", runs[0].FailedStep)
	}
	if runs[0].FailureReason != "Run tests" {
		t.Fatalf("FailureReason = %q, want Run tests", runs[0].FailureReason)
	}
}

func TestEnrichWorkflowMetrics_PreservesRunOnWorkflowJobsError(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/test-org/repo-a/actions/runs/101/attempts/2/jobs":
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	runs, err := provider.EnrichWorkflowMetrics(context.Background(), []domain.WorkflowMetrics{{
		RunID:       101,
		RunAttempt:  2,
		Repo:        "repo-a",
		Workflow:    "build",
		Conclusion:  "failure",
		DurationS:   30,
		RunNumber:   7,
		Event:       "push",
		Branch:      "main",
		CompletedAt: "2026-04-19T12:01:30Z",
	}})
	if err == nil {
		t.Fatal("expected enrichment error, got nil")
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].FailedJob != "" {
		t.Fatalf("FailedJob = %q, want empty", runs[0].FailedJob)
	}
	if runs[0].FailedStep != "" {
		t.Fatalf("FailedStep = %q, want empty", runs[0].FailedStep)
	}
	if runs[0].FailureReason != "failure" {
		t.Fatalf("FailureReason = %q, want failure", runs[0].FailureReason)
	}
}

func testProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	return testProviderWithPrefix(t, server, "auto")
}

func testProviderWithPrefix(t *testing.T, server *httptest.Server, prefix string) *Provider {
	t.Helper()

	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	client := gh.NewClient(server.Client()).WithAuthToken("token")
	client.BaseURL = baseURL
	return newProvider(client, "test-org", prefix)
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
