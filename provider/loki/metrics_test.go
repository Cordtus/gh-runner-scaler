package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
