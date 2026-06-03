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
	requests := 0
	var captured lokiPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend := New(server.URL, "user", "key", "Axionic-Labs")
	runs := []domain.WorkflowMetrics{
		{RunID: 2, Repo: "repo-a", Workflow: "build", Conclusion: "success", DurationS: 90, RunNumber: 7, Event: "push", Branch: "main", CompletedAt: "2026-05-04T12:05:00Z"},
		{RunID: 3, Repo: "repo-b", Workflow: "lint", Conclusion: "failure", DurationS: 45, RunNumber: 8, Event: "pull_request", Branch: "dev", CompletedAt: "2026-05-04T12:03:00Z"},
		{RunID: 1, Repo: "repo-a", Workflow: "test", Conclusion: "success", DurationS: 30, RunNumber: 6, Event: "push", Branch: "main", CompletedAt: "2026-05-04T12:01:00Z"},
	}

	if err := backend.PushWorkflowMetrics(context.Background(), runs); err != nil {
		t.Fatalf("PushWorkflowMetrics returned error: %v", err)
	}

	if requests != 1 {
		t.Fatalf("request count = %d, want 1", requests)
	}

	if len(captured.Streams) != 2 {
		t.Fatalf("streams len = %d, want 2", len(captured.Streams))
	}

	streamsByRepo := make(map[string]lokiStream, len(captured.Streams))
	for i, stream := range captured.Streams {
		if got := stream.Stream["service"]; got != "workflow-metrics" {
			t.Fatalf("stream %d service label = %q, want workflow-metrics", i, got)
		}
		streamsByRepo[stream.Stream["repo"]] = stream
	}

	repoA, ok := streamsByRepo["repo-a"]
	if !ok {
		t.Fatal("repo-a stream missing")
	}
	if len(repoA.Values) != 2 {
		t.Fatalf("repo-a values len = %d, want 2", len(repoA.Values))
	}
	for i, value := range repoA.Values {
		if len(value) != 2 {
			t.Fatalf("repo-a values[%d] len = %d, want 2", i, len(value))
		}
		var decoded domain.WorkflowMetrics
		if err := json.Unmarshal([]byte(value[1]), &decoded); err != nil {
			t.Fatalf("unmarshal repo-a value %d: %v", i, err)
		}
		if decoded.RunID != int64(i+1) {
			t.Fatalf("repo-a decoded run %d id = %d, want %d", i, decoded.RunID, i+1)
		}
		gotNS, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			t.Fatalf("parse repo-a timestamp %d: %v", i, err)
		}
		wantTime, err := time.Parse(time.RFC3339, decoded.CompletedAt)
		if err != nil {
			t.Fatalf("parse repo-a completed_at %d: %v", i, err)
		}
		if gotNS != wantTime.UnixNano()+int64(i) {
			t.Fatalf("repo-a timestamp %d = %d, want %d", i, gotNS, wantTime.UnixNano()+int64(i))
		}
	}

	repoB, ok := streamsByRepo["repo-b"]
	if !ok {
		t.Fatal("repo-b stream missing")
	}
	if len(repoB.Values) != 1 {
		t.Fatalf("repo-b values len = %d, want 1", len(repoB.Values))
	}
	var repoBRun domain.WorkflowMetrics
	if err := json.Unmarshal([]byte(repoB.Values[0][1]), &repoBRun); err != nil {
		t.Fatalf("unmarshal repo-b value: %v", err)
	}
	if repoBRun.RunID != 3 {
		t.Fatalf("repo-b decoded run id = %d, want 3", repoBRun.RunID)
	}
}

func TestPushWorkflowMetrics_SortsEqualAndMissingCompletedAtDeterministically(t *testing.T) {
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
		{RunID: 3, Repo: "repo-a", Workflow: "build", Conclusion: "success", DurationS: 60, RunNumber: 9, Event: "push", Branch: "main"},
		{RunID: 2, Repo: "repo-a", Workflow: "build", Conclusion: "success", DurationS: 55, RunNumber: 8, Event: "push", Branch: "main", CompletedAt: "2026-05-04T12:05:00Z"},
		{RunID: 1, Repo: "repo-a", Workflow: "build", Conclusion: "success", DurationS: 50, RunNumber: 7, Event: "push", Branch: "main", CompletedAt: "2026-05-04T12:05:00Z"},
	}

	if err := backend.PushWorkflowMetrics(context.Background(), runs); err != nil {
		t.Fatalf("PushWorkflowMetrics returned error: %v", err)
	}

	if len(captured.Streams) != 1 {
		t.Fatalf("streams len = %d, want 1", len(captured.Streams))
	}
	if len(captured.Streams[0].Values) != 3 {
		t.Fatalf("values len = %d, want 3", len(captured.Streams[0].Values))
	}

	var ordered []domain.WorkflowMetrics
	for i, value := range captured.Streams[0].Values {
		var decoded domain.WorkflowMetrics
		if err := json.Unmarshal([]byte(value[1]), &decoded); err != nil {
			t.Fatalf("unmarshal value %d: %v", i, err)
		}
		ordered = append(ordered, decoded)
	}

	if ordered[0].RunID != 1 || ordered[1].RunID != 2 || ordered[2].RunID != 3 {
		t.Fatalf("ordered run IDs = [%d %d %d], want [1 2 3]", ordered[0].RunID, ordered[1].RunID, ordered[2].RunID)
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

func TestPushRunnerMetrics_LabelsGroupID(t *testing.T) {
	var captured lokiPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend := New(server.URL, "user", "key", "Axionic-Labs")
	metrics := domain.RunnerMetrics{
		GroupID:      "rust",
		TotalRunners: 1,
	}

	if err := backend.PushRunnerMetrics(context.Background(), metrics); err != nil {
		t.Fatalf("PushRunnerMetrics returned error: %v", err)
	}

	if len(captured.Streams) != 1 {
		t.Fatalf("Streams len = %d, want 1", len(captured.Streams))
	}
	if got := captured.Streams[0].Stream["group_id"]; got != "rust" {
		t.Fatalf("group_id label = %q, want rust", got)
	}
}

func TestPushHostMetrics_LabelsGroupID(t *testing.T) {
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
		GroupID:           "typescript",
		ContainersRunning: 2,
	}

	if err := backend.PushHostMetrics(context.Background(), metrics); err != nil {
		t.Fatalf("PushHostMetrics returned error: %v", err)
	}

	if len(captured.Streams) != 1 {
		t.Fatalf("Streams len = %d, want 1", len(captured.Streams))
	}
	if got := captured.Streams[0].Stream["group_id"]; got != "typescript" {
		t.Fatalf("group_id label = %q, want typescript", got)
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
