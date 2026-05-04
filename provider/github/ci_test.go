package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	client := gh.NewClient(server.Client()).WithAuthToken("token")
	client.BaseURL = baseURL
	return newProvider(client, "test-org", "auto")
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
