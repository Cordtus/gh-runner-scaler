package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// runWebhookServer starts the HTTP webhook listener and blocks until ctx is cancelled.
func (d *Daemon) runWebhookServer(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleWebhook)

	addr := fmt.Sprintf(":%d", d.cfg.WebhookPort)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
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
type debouncer struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

var webhookDebouncer = &debouncer{
	timers: make(map[string]*time.Timer),
}

// schedule queues fn to run after delay, resetting on rapid calls with the same key.
func (db *debouncer) schedule(key string, delay time.Duration, fn func()) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if existing, ok := db.timers[key]; ok {
		existing.Stop()
	}
	db.timers[key] = time.AfterFunc(delay, fn)
}

func (d *Daemon) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("gh-runner-scaler webhook listener"))
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

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
		// Unrecognized or ignored event type.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	// Dispatch.
	switch event.Type {
	case domain.EventJobQueued, domain.EventJobCompleted:
		d.log.Info("webhook event", "detail", event.Detail)
		webhookDebouncer.schedule("scaler", d.cfg.WebhookDebounce, func() {
			d.Trigger()
		})

	case domain.EventPush:
		d.handlePushEvent(event)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handlePushEvent triggers a cache sync if the pushed repo is tracked.
func (d *Daemon) handlePushEvent(event *domain.WebhookEvent) {
	if event.Ref != "refs/heads/main" {
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

	d.log.Info("push to tracked repo", "detail", event.Detail, "cache_path", cachePath)

	webhookDebouncer.schedule("sync-"+repoName, d.cfg.WebhookDebounce, func() {
		d.syncCacheRepo(context.Background(), event.Repo, cachePath)
	})
}

// syncCacheRepo syncs a dependency repo in the cache volume by exec'ing into
// an already-running container. Replaces sync-cache-deps.sh.
func (d *Daemon) syncCacheRepo(ctx context.Context, repo, cachePath string) {
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

	script := fmt.Sprintf(
		"git config --global --add safe.directory '%s' 2>/dev/null; git -C '%s' fetch origin main 2>&1; git -C '%s' reset --hard origin/main 2>&1",
		cachePath, cachePath, cachePath,
	)

	_, err = d.runtime.ExecCommand(ctx, target, []string{"bash", "-c", script})
	if err != nil {
		d.log.Error("cache sync failed", "repo", repo, "container", target, "error", err)
		return
	}

	log := d.log.With("repo", repo, "container", target)
	log.Info("cache sync completed")
}

// Compile-time check that slog.Logger is used.
var _ *slog.Logger
