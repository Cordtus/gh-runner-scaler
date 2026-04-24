package github

import (
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

func TestParseWorkflowJob_UnsupportedActionsRemainIgnored(t *testing.T) {
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
	if event != nil {
		t.Fatalf("expected unsupported workflow_job action to be ignored, got %#v", event)
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
}
