// Package domain defines shared value types used across the scaler.
// No package in domain/ imports from provider/ or engine/.
package domain

import "time"

// ContainerStatus represents the runtime state of a container.
type ContainerStatus int

const (
	StatusUnknown ContainerStatus = iota
	StatusRunning
	StatusStopped
)

func (s ContainerStatus) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Container is a minimal representation of a managed container.
type Container struct {
	Name   string
	Status ContainerStatus
}

// Runner represents a GitHub Actions runner (or equivalent CI runner).
type Runner struct {
	ID     int64
	Name   string
	Status string // "online" / "offline"
	Busy   bool
	Labels []string
	IsAuto bool
}

// RunnerSnapshot is a point-in-time summary of the runner pool.
type RunnerSnapshot struct {
	Total     int
	Busy      int
	Idle      int
	Online    int
	Offline   int
	Auto      int
	Permanent int
	Runners   []Runner
}

// WebhookEventType classifies incoming webhook events.
type WebhookEventType int

const (
	EventUnknown WebhookEventType = iota
	EventJobQueued
	EventJobCompleted
	EventPush
)

// WebhookEvent is a provider-agnostic representation of a webhook payload.
type WebhookEvent struct {
	Type          WebhookEventType
	EventType     string
	Action        string
	Repo          string // e.g. "Axionic-Labs/axionic-ui"
	Ref           string // e.g. "refs/heads/main"
	DefaultBranch string // e.g. "main"
	Branch        string
	Commit        string
	Workflow      string
	Job           string
	Runner        string
	Status        string
	Conclusion    string
	RunID         int64
	RunAttempt    int
	Detail        string // human-readable summary for logging
}

// RunnerDetail is a per-runner entry in metrics payloads.
type RunnerDetail struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
	IsAuto bool   `json:"is_auto"`
}

// RunnerMetrics is the runner pool state pushed to the metrics backend.
// JSON field names must match the Grafana dashboard queries exactly.
type RunnerMetrics struct {
	TotalRunners           int            `json:"total_runners"`
	BusyRunners            int            `json:"busy_runners"`
	IdleRunners            int            `json:"idle_runners"`
	AvailableOnlineRunners int            `json:"available_online_runners"`
	OnlineRunners          int            `json:"online_runners"`
	OfflineRunners         int            `json:"offline_runners"`
	AutoRunners            int            `json:"auto_runners"`
	PermanentRunners       int            `json:"permanent_runners"`
	UtilizationPct         float64        `json:"utilization_pct"`
	Runners                []RunnerDetail `json:"runners"`
}

// WorkflowMetrics captures a single completed workflow run.
type WorkflowMetrics struct {
	RunID       int64  `json:"run_id,omitempty"`
	RunAttempt  int    `json:"run_attempt,omitempty"`
	Repo        string `json:"repo"`
	Workflow    string `json:"workflow"`
	Conclusion  string `json:"conclusion"`
	DurationS   int    `json:"duration_s"`
	RunNumber   int    `json:"run_number"`
	Event       string `json:"event"`
	Branch      string `json:"branch"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// HostMetrics captures container and storage pool state.
type HostMetrics struct {
	ContainersRunning       int     `json:"containers_running"`
	ContainersStopped       int     `json:"containers_stopped"`
	RunnerContainersRunning *int    `json:"runner_containers_running,omitempty"`
	RunnerContainersStopped *int    `json:"runner_containers_stopped,omitempty"`
	CachePoolUsedGB         float64 `json:"cache_pool_used_gb,omitempty"`
	CachePoolTotalGB        float64 `json:"cache_pool_total_gb,omitempty"`
	CachePoolPct            float64 `json:"cache_pool_pct,omitempty"`
}

// ContainerState tracks scaler-managed state for a single container.
type ContainerState struct {
	Name       string
	LastActive time.Time
}

// LogEntry captures a structured daemon/reconciler log entry persisted for API access.
type LogEntry struct {
	Time       time.Time         `json:"time"`
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	Component  string            `json:"component,omitempty"`
	EventType  string            `json:"event_type,omitempty"`
	Action     string            `json:"action,omitempty"`
	Repo       string            `json:"repo,omitempty"`
	Workflow   string            `json:"workflow,omitempty"`
	Job        string            `json:"job,omitempty"`
	Runner     string            `json:"runner,omitempty"`
	Container  string            `json:"container,omitempty"`
	Commit     string            `json:"commit,omitempty"`
	Branch     string            `json:"branch,omitempty"`
	Status     string            `json:"status,omitempty"`
	Conclusion string            `json:"conclusion,omitempty"`
	Detail     string            `json:"detail,omitempty"`
	CachePath  string            `json:"cache_path,omitempty"`
	RunID      int64             `json:"run_id,omitempty"`
	RunAttempt int               `json:"run_attempt,omitempty"`
	Error      string            `json:"error,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}
