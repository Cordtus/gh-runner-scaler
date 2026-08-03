package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const maxWebhookBodyBytes = 1 << 20

// runWebhookServer starts the HTTP webhook listener and blocks until ctx is cancelled.
func (d *Daemon) runWebhookServer(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/logs", d.handleLogs)
	mux.HandleFunc("/healthz", d.handleHealth)
	mux.HandleFunc("/statusz", d.handleStatus)
	mux.HandleFunc("/", d.handleWebhook)

	addr := fmt.Sprintf(":%d", d.cfg.WebhookPort)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutCtx)
	}()

	d.log.Info("webhook server listening", "addr", addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		d.log.Error("webhook server error", "error", err)
	}
}

// debouncer manages per-key debounced triggers.
type debounceEntry struct {
	generation uint64
	timer      *time.Timer
}

type debouncer struct {
	mu     sync.Mutex
	timers map[string]*debounceEntry
}

func newDebouncer() *debouncer {
	return &debouncer{
		timers: make(map[string]*debounceEntry),
	}
}

// schedule queues fn to run after delay, resetting on rapid calls with the same key.
func (db *debouncer) schedule(ctx context.Context, key string, delay time.Duration, fn func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if delay < 0 {
		delay = 0
	}

	db.mu.Lock()
	entry, ok := db.timers[key]
	if !ok {
		entry = &debounceEntry{}
		db.timers[key] = entry
	}
	entry.generation++
	generation := entry.generation
	if entry.timer != nil {
		entry.timer.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		db.mu.Lock()
		current, ok := db.timers[key]
		if !ok || current.generation != generation {
			db.mu.Unlock()
			return
		}
		delete(db.timers, key)
		db.mu.Unlock()

		if ctx.Err() == nil {
			fn()
		}

		db.mu.Lock()
		current, ok = db.timers[key]
		if ok && current.generation == generation {
			delete(db.timers, key)
		}
		db.mu.Unlock()
	})
	entry.timer = timer
	db.mu.Unlock()
}

func (d *Daemon) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("gh-runner-scaler webhook listener (/logs requires bearer auth)"))
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Validate signature.
	signature := r.Header.Get("X-Hub-Signature-256")
	if err := d.ci.ValidateWebhookPayload(payload, signature); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Parse event.
	eventType := r.Header.Get("X-GitHub-Event")
	event, err := d.ci.ParseWebhookEvent(eventType, payload)
	if err != nil {
		d.log.Warn("failed to parse webhook event", "type", eventType, "error", err)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	if event == nil {
		d.recordWebhook(eventType, nil)
		// Unrecognized or ignored event type.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	d.recordWebhook(eventType, event)

	// Dispatch.
	d.logWebhookEvent(event)

	switch event.Type {
	case domain.EventJobQueued, domain.EventJobInProgress, domain.EventJobCompleted:
		for _, group := range d.groupsForEvent(event) {
			groupID := group.ID
			if d.demand != nil {
				d.demandMu.Lock()
				var demandErr error
				if event.Type == domain.EventJobQueued {
					demandErr = d.demand.Queue(groupID, domain.QueuedJob{
						ID:       event.JobID,
						Repo:     event.Repo,
						Labels:   append([]string(nil), event.Labels...),
						QueuedAt: time.Now().UTC(),
					})
				} else {
					demandErr = d.demand.Clear(groupID, event.JobID)
				}
				d.demandMu.Unlock()
				if demandErr != nil {
					d.log.Error("failed to persist workflow demand", "runner_group", groupID, "job_id", event.JobID, "error", demandErr)
				}
			}
			d.debouncer.schedule(d.currentLifecycleContext(), "scaler-"+groupID, d.cfg.WebhookDebounce, func() {
				d.TriggerGroup(groupID)
			})
		}
	case domain.EventPush:
		d.handlePushEvent(event)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.logStore == nil {
		http.Error(w, "log store unavailable", http.StatusServiceUnavailable)
		return
	}
	if !d.authorizeLogsRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	query, err := parseLogQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response := logResponse{
		Entries: d.logStore.Query(query),
	}
	response.Count = len(response.Entries)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		d.log.Error("failed to encode log response", "event_type", "logs", "action", "encode_failed", "error", err)
	}
}

func (d *Daemon) authorizeLogsRequest(r *http.Request) bool {
	token := strings.TrimSpace(d.cfg.LogsToken)
	if token == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	provided := auth[len("Bearer "):]
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (d *Daemon) logWebhookEvent(event *domain.WebhookEvent) {
	if event == nil {
		return
	}
	args := []any{
		"event_type", event.EventType,
		"action", event.Action,
		"repo", event.Repo,
		"detail", event.Detail,
	}
	if event.Workflow != "" {
		args = append(args, "workflow", event.Workflow)
	}
	if event.Job != "" {
		args = append(args, "job", event.Job)
	}
	if event.JobID != 0 {
		args = append(args, "job_id", event.JobID)
	}
	if event.Runner != "" {
		args = append(args, "runner", event.Runner)
	}
	if event.Commit != "" {
		args = append(args, "commit", event.Commit)
	}
	if event.Branch != "" {
		args = append(args, "branch", event.Branch)
	}
	if event.Status != "" {
		args = append(args, "status", event.Status)
	}
	if event.Conclusion != "" {
		args = append(args, "conclusion", event.Conclusion)
	}
	if event.RunID != 0 {
		args = append(args, "run_id", event.RunID)
	}
	if event.RunAttempt != 0 {
		args = append(args, "run_attempt", event.RunAttempt)
	}
	if len(event.Labels) > 0 {
		args = append(args, "labels", strings.Join(event.Labels, ","))
	}
	d.log.Info("webhook event", args...)
}

func (d *Daemon) groupsForEvent(event *domain.WebhookEvent) []RunnerGroup {
	if event == nil {
		return append([]RunnerGroup(nil), d.groups...)
	}

	candidates := d.groupsForRepo(event.Repo)
	if len(candidates) == 0 {
		return nil
	}
	if len(event.Labels) == 0 {
		return candidates
	}

	groups := make([]RunnerGroup, 0, len(candidates))
	for _, group := range candidates {
		if labelsMatch(event.Labels, group.MatchLabels) {
			groups = append(groups, group)
		}
	}
	if len(groups) == 0 {
		return candidates
	}
	return groups
}

func (d *Daemon) groupsForRepo(repo string) []RunnerGroup {
	if strings.TrimSpace(repo) == "" {
		return append([]RunnerGroup(nil), d.groups...)
	}

	groups := make([]RunnerGroup, 0, len(d.groups))
	for _, group := range d.groups {
		if groupMatchesRepo(group, repo) {
			groups = append(groups, group)
		}
	}
	return groups
}

func groupMatchesRepo(group RunnerGroup, repo string) bool {
	target := strings.ToLower(strings.TrimSpace(group.Target))
	repo = strings.ToLower(strings.TrimSpace(repo))
	if target == "" || repo == "" {
		return true
	}
	if group.RepoScoped {
		return repo == target
	}
	owner, _, ok := strings.Cut(repo, "/")
	return ok && owner == target
}

func labelsMatch(jobLabels, groupLabels []string) bool {
	if len(groupLabels) == 0 {
		return true
	}
	seen := make(map[string]struct{}, len(jobLabels))
	for _, label := range jobLabels {
		seen[strings.ToLower(strings.TrimSpace(label))] = struct{}{}
	}
	for _, label := range groupLabels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		if _, ok := seen[label]; !ok {
			return false
		}
	}
	return true
}

// handlePushEvent triggers a cache sync if the pushed repo is tracked.
func (d *Daemon) handlePushEvent(event *domain.WebhookEvent) {
	branch, ok := cacheSyncBranch(event)
	if !ok {
		return
	}

	cachePath, ok := d.cfg.SyncRepos[event.Repo]
	if !ok {
		return
	}

	repoName := event.Repo
	if idx := strings.LastIndex(repoName, "/"); idx >= 0 {
		repoName = repoName[idx+1:]
	}

	d.log.Info(
		"push to tracked repo",
		"event_type", "cache_sync",
		"action", "scheduled",
		"repo", event.Repo,
		"branch", branch,
		"commit", event.Commit,
		"detail", event.Detail,
		"cache_path", cachePath,
	)

	d.debouncer.schedule(d.currentLifecycleContext(), "sync-"+repoName, d.cfg.WebhookDebounce, func() {
		d.syncCacheRepo(d.currentLifecycleContext(), event.Repo, branch, cachePath)
	})
}

func cacheSyncBranch(event *domain.WebhookEvent) (string, bool) {
	if event == nil || event.DefaultBranch == "" {
		return "", false
	}
	expectedRef := "refs/heads/" + event.DefaultBranch
	if event.Ref != expectedRef {
		return "", false
	}
	return event.DefaultBranch, true
}

// syncCacheRepo syncs a dependency repo in the cache volume by exec'ing into
// an already-running container. Replaces sync-cache-deps.sh.
func (d *Daemon) syncCacheRepo(ctx context.Context, repo, branch, cachePath string) {
	// Find a running container to exec into.
	containers, err := d.runtime.ListContainers(ctx, "")
	if err != nil {
		d.log.Error("failed to list containers for sync", "error", err)
		return
	}

	var target string
	for _, c := range containers {
		if c.Status != domain.StatusRunning {
			continue
		}
		// Prefer the permanent runner.
		if c.Name == "gh-runner" {
			target = c.Name
			break
		}
		if target == "" {
			target = c.Name
		}
	}

	if target == "" {
		d.log.Error("no running containers for cache sync")
		return
	}

	script := strings.Join([]string{
		"set -eu",
		"git config --global --add safe.directory " + shellQuote(cachePath),
		"git -C " + shellQuote(cachePath) + " fetch --prune origin " + shellQuote(branch),
		"git -C " + shellQuote(cachePath) + " reset --hard FETCH_HEAD",
	}, "\n")
	if _, err = d.runtime.ExecCommand(ctx, target, []string{"bash", "-c", script}); err != nil {
		d.log.Error(
			"cache sync failed",
			"event_type", "cache_sync",
			"action", "failed",
			"repo", repo,
			"branch", branch,
			"container", target,
			"runner", target,
			"error", err,
		)
		return
	}

	log := d.log.With("repo", repo, "branch", branch, "container", target)
	log.Info("cache sync completed", "event_type", "cache_sync", "action", "completed", "runner", target)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// Compile-time check that slog.Logger is used.
var _ *slog.Logger
