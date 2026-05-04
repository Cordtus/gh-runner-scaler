package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestPushWorkflowMetrics_SendsIndividualLogEntries(t *testing.T) {
	var captured lokiPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend := New(server.URL, "user", "key", "Axionic-Labs")
	runs := []domain.WorkflowMetrics{
		{Repo: "repo-a", Workflow: "build", Conclusion: "success", DurationS: 90, RunNumber: 7, Event: "push", Branch: "main"},
		{Repo: "repo-b", Workflow: "lint", Conclusion: "failure", DurationS: 45, RunNumber: 8, Event: "pull_request", Branch: "dev"},
	}

	if err := backend.PushWorkflowMetrics(context.Background(), runs); err != nil {
		t.Fatalf("PushWorkflowMetrics returned error: %v", err)
	}

	if len(captured.Streams) != 1 {
		t.Fatalf("Streams len = %d, want 1", len(captured.Streams))
	}
	stream := captured.Streams[0]
	if got := stream.Stream["service"]; got != "workflow-metrics" {
		t.Fatalf("service label = %q, want workflow-metrics", got)
	}
	if len(stream.Values) != len(runs) {
		t.Fatalf("Values len = %d, want %d", len(stream.Values), len(runs))
	}

	for i, value := range stream.Values {
		if len(value) != 2 {
			t.Fatalf("Values[%d] len = %d, want 2", i, len(value))
		}
		var decoded domain.WorkflowMetrics
		if err := json.Unmarshal([]byte(value[1]), &decoded); err != nil {
			t.Fatalf("unmarshal value %d: %v", i, err)
		}
		if decoded != runs[i] {
			t.Fatalf("decoded run %d = %+v, want %+v", i, decoded, runs[i])
		}
	}
}

func TestPushHostMetrics_OmitsManagedRunnerFieldsWhenUnavailable(t *testing.T) {
	var captured lokiPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend := New(server.URL, "user", "key", "Axionic-Labs")
	metrics := domain.HostMetrics{
		ContainersRunning: 3,
		ContainersStopped: 12,
	}

	if err := backend.PushHostMetrics(context.Background(), metrics); err != nil {
		t.Fatalf("PushHostMetrics returned error: %v", err)
	}

	if len(captured.Streams) != 1 {
		t.Fatalf("Streams len = %d, want 1", len(captured.Streams))
	}
	stream := captured.Streams[0]
	if got := stream.Stream["service"]; got != "host-metrics" {
		t.Fatalf("service label = %q, want host-metrics", got)
	}
	if len(stream.Values) != 1 {
		t.Fatalf("Values len = %d, want 1", len(stream.Values))
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(stream.Values[0][1]), &decoded); err != nil {
		t.Fatalf("unmarshal host metrics value: %v", err)
	}
	if _, ok := decoded["runner_containers_running"]; ok {
		t.Fatal("runner_containers_running present, want omitted")
	}
	if _, ok := decoded["runner_containers_stopped"]; ok {
		t.Fatal("runner_containers_stopped present, want omitted")
	}
}

func TestPushIssueEvents_UsesObservedTimestamp(t *testing.T) {
	var captured lokiPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend := New(server.URL, "user", "key", "Axionic-Labs")
	observedAt := "2026-05-04T12:34:56Z"
	issues := []domain.IssueEvent{{
		Level:      "warn",
		Kind:       "daemon",
		Reason:     "rate limit exceeded",
		Message:    "failed to collect workflow metrics",
		ObservedAt: observedAt,
	}}

	if err := backend.PushIssueEvents(context.Background(), issues); err != nil {
		t.Fatalf("PushIssueEvents returned error: %v", err)
	}

	if len(captured.Streams) != 1 || len(captured.Streams[0].Values) != 1 {
		t.Fatalf("unexpected captured payload: %+v", captured)
	}
	gotNS, err := strconv.ParseInt(captured.Streams[0].Values[0][0], 10, 64)
	if err != nil {
		t.Fatalf("parse timestamp: %v", err)
	}
	wantTime, _ := time.Parse(time.RFC3339, observedAt)
	if gotNS != wantTime.UnixNano() {
		t.Fatalf("timestamp = %d, want %d", gotNS, wantTime.UnixNano())
	}
}
