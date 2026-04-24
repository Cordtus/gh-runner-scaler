package daemon

import (
	"testing"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestCacheSyncBranch_DefaultBranchPush(t *testing.T) {
	event := &domain.WebhookEvent{
		Ref:           "refs/heads/trunk",
		DefaultBranch: "trunk",
	}

	branch, ok := cacheSyncBranch(event)
	if !ok {
		t.Fatal("expected default-branch push to trigger cache sync")
	}
	if branch != "trunk" {
		t.Fatalf("expected trunk branch, got %q", branch)
	}
}

func TestCacheSyncBranch_NonDefaultBranchPush(t *testing.T) {
	event := &domain.WebhookEvent{
		Ref:           "refs/heads/feature-x",
		DefaultBranch: "main",
	}

	if branch, ok := cacheSyncBranch(event); ok {
		t.Fatalf("expected non-default branch push to be ignored, got %q", branch)
	}
}

func TestCacheSyncBranch_NilEvent(t *testing.T) {
	if branch, ok := cacheSyncBranch(nil); ok {
		t.Fatalf("expected nil event to be ignored, got %q", branch)
	}
}
