package runnerobs

import (
	"strings"
	"testing"
)

func TestRenderConfig_IsCredentialFreeAndBounded(t *testing.T) {
	got, err := RenderConfig(Config{PushURL: "http://loki.internal/loki/api/v1/push", MaxRetries: 3}, Runner{GroupID: "node", Container: "gh-runner-node-1", Target: "Cordtus/poolbet"})
	if err != nil {
		t.Fatalf("RenderConfig error: %v", err)
	}
	for _, want := range []string{"runner_group: node", "runner: gh-runner-node-1", "repo: Cordtus/poolbet", "/home/runner/_diag/Worker_*.log", "/var/lib/promtail/positions.yaml", "max_retries: 3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "basic_auth") || strings.Contains(got, "glc_") {
		t.Fatalf("config contains credentials:\n%s", got)
	}
}

func TestClassifyStatus(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		if ClassifyStatus(code) != Permanent {
			t.Fatalf("status %d = %v, want permanent", code, ClassifyStatus(code))
		}
	}
	for _, code := range []int{429, 500, 502, 503} {
		if ClassifyStatus(code) != Transient {
			t.Fatalf("status %d = %v, want transient", code, ClassifyStatus(code))
		}
	}

}
