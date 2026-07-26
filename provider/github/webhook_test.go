package github

import (
	"reflect"
	"testing"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestParseWorkflowJob_QueuedAction(t *testing.T) {
	payload := []byte(`{
		"action":"queued",
		"workflow_job":{
			"name":"quality"
		},
		"repository":{
			"full_name":"Acme/repo"
		}
	}`)

	event, err := parseWorkflowJob(payload)
	if err != nil {
		t.Fatalf("parseWorkflowJob returned error: %v", err)
	}
	if event == nil {
		t.Fatal("expected workflow_job event")
	}
	if event.Type != domain.EventJobQueued {
		t.Fatalf("expected queued event type, got %v", event.Type)
	}
}

func TestParseWorkflowJob_CompletedAction(t *testing.T) {
	payload := []byte(`{
		"action":"completed",
		"workflow_job":{
			"name":"docs"
		},
		"repository":{
			"full_name":"Acme/repo"
		}
	}`)

	event, err := parseWorkflowJob(payload)
	if err != nil {
		t.Fatalf("parseWorkflowJob returned error: %v", err)
	}
	if event == nil {
		t.Fatal("expected workflow_job event")
	}
	if event.Type != domain.EventJobCompleted {
		t.Fatalf("expected completed event type, got %v", event.Type)
	}
}

func TestParseWorkflowJob_MissingLabelsStillParses(t *testing.T) {
	payload := []byte(`{
		"action":"queued",
		"workflow_job":{
			"name":"quality"
		},
		"repository":{
			"full_name":"Acme/repo"
		}
	}`)

	event, err := parseWorkflowJob(payload)
	if err != nil {
		t.Fatalf("parseWorkflowJob returned error: %v", err)
	}
	if event == nil {
		t.Fatal("expected workflow_job event")
	}
	if event.Detail != "queued: Acme/repo / quality" {
		t.Fatalf("expected detail to survive missing labels, got %q", event.Detail)
	}
}

func TestParseWorkflowJob_CapturesRunnerLabels(t *testing.T) {
	payload := []byte(`{
		"action":"queued",
		"workflow_job":{
			"name":"quality",
			"labels":["self-hosted","linux","x64","rust"]
		},
		"repository":{
			"full_name":"Acme/repo"
		}
	}`)

	event, err := parseWorkflowJob(payload)
	if err != nil {
		t.Fatalf("parseWorkflowJob returned error: %v", err)
	}
	want := []string{"self-hosted", "linux", "x64", "rust"}
	if got := event.Labels; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected workflow job labels, got %v", got)
	}
}

func TestParseWorkflowJob_InProgressActionRemainsObservable(t *testing.T) {
	payload := []byte(`{
		"action":"in_progress",
		"workflow_job":{
			"name":"quality",
			"labels":["self-hosted"]
		},
		"repository":{
			"full_name":"Acme/repo"
		}
	}`)

	event, err := parseWorkflowJob(payload)
	if err != nil {
		t.Fatalf("parseWorkflowJob returned error: %v", err)
	}
	if event == nil {
		t.Fatal("expected in_progress workflow_job event")
	}
	if event.Type != domain.EventJobInProgress {
		t.Fatalf("in_progress event type = %v, want EventJobInProgress", event.Type)
	}
}

func TestParseWorkflowJob_TracksRunnerWorkflowAndCommit(t *testing.T) {
	payload := []byte(`{
		"action":"completed",
		"repository":{"full_name":"Acme/repo"},
		"workflow_job":{
			"id":991,
			"name":"integration",
			"workflow_name":"CI",
			"head_branch":"main",
			"head_sha":"0123456789abcdef0123456789abcdef01234567",
			"runner_name":"gh-runner-auto-3",
			"status":"completed",
			"conclusion":"success",
			"run_id":451,
			"run_attempt":2
		}
	}`)

	event, err := parseWorkflowJob(payload)
	if err != nil {
		t.Fatalf("parseWorkflowJob returned error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.EventType != "workflow_job" {
		t.Fatalf("expected workflow_job event type, got %q", event.EventType)
	}
	if event.Action != "completed" {
		t.Fatalf("expected completed action, got %q", event.Action)
	}
	if event.Workflow != "CI" {
		t.Fatalf("expected workflow CI, got %q", event.Workflow)
	}
	if event.Job != "integration" {
		t.Fatalf("expected job integration, got %q", event.Job)
	}
	if event.JobID != 991 {
		t.Fatalf("expected job id 991, got %d", event.JobID)
	}
	if event.Runner != "gh-runner-auto-3" {
		t.Fatalf("expected runner gh-runner-auto-3, got %q", event.Runner)
	}
	if event.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("expected full commit hash, got %q", event.Commit)
	}
	if event.Branch != "main" {
		t.Fatalf("expected branch main, got %q", event.Branch)
	}
	if event.RunID != 451 {
		t.Fatalf("expected run id 451, got %d", event.RunID)
	}
	if event.RunAttempt != 2 {
		t.Fatalf("expected run attempt 2, got %d", event.RunAttempt)
	}
}

func TestParsePush_TracksDefaultBranch(t *testing.T) {
	payload := []byte(`{
		"ref":"refs/heads/trunk",
		"after":"0123456789abcdef",
		"repository":{
			"full_name":"Acme/repo",
			"default_branch":"trunk"
		}
	}`)

	event, err := parsePush(payload)
	if err != nil {
		t.Fatalf("parsePush returned error: %v", err)
	}
	if event.DefaultBranch != "trunk" {
		t.Fatalf("expected default branch trunk, got %q", event.DefaultBranch)
	}
	if event.Ref != "refs/heads/trunk" {
		t.Fatalf("expected push ref preserved, got %q", event.Ref)
	}
	if event.Branch != "trunk" {
		t.Fatalf("expected branch trunk, got %q", event.Branch)
	}
	if event.Commit != "0123456789abcdef" {
		t.Fatalf("expected full commit hash, got %q", event.Commit)
	}
}
