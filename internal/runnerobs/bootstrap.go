package runnerobs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
)

// Executor is the narrow container boundary required by Bootstrapper.
type Executor interface {
	ExecCommand(context.Context, string, []string) (string, error)
}

// Bootstrapper installs and starts observability only after runner registration.
type Bootstrapper struct {
	Executor  Executor
	Config    Config
	HealthURL string
	GroupID   string
	Target    string
}

func (b Bootstrapper) Prepare(ctx context.Context, container string) error {
	if b.Executor == nil {
		return fmt.Errorf("runner log bootstrapper has no container executor")
	}
	if err := Preflight(ctx, b.HealthURL, b.Config.MaxRetries, time.Second, time.Minute); err != nil {
		return err
	}
	contents, err := RenderConfig(b.Config, Runner{GroupID: b.GroupID, Container: container, Target: b.Target})
	if err != nil {
		return err
	}
	payload := base64.StdEncoding.EncodeToString([]byte(contents))
	write := []string{"bash", "-ceu", "install -d -m 0755 /etc/gh-runner-observability /var/lib/promtail; printf %s '" + payload + "' | base64 -d > /etc/gh-runner-observability/promtail.yml"}
	if _, err := b.Executor.ExecCommand(ctx, container, write); err != nil {
		return fmt.Errorf("installing runner log config: %w", err)
	}
	if _, err := b.Executor.ExecCommand(ctx, container, []string{"systemctl", "enable", "--now", "promtail.service"}); err != nil {
		return fmt.Errorf("starting runner log shipper: %w", err)
	}
	return nil
}

type statusError struct{ code int }

func (e statusError) Error() string {
	return fmt.Sprintf("runner log endpoint returned HTTP %d", e.code)
}

// Preflight probes the endpoint with finite retries. Permanent 4xx failures
// return immediately; only transient transport, 429, and 5xx failures retry.
func Preflight(ctx context.Context, endpoint string, retries int, initial, maximum time.Duration) error {
	if endpoint == "" {
		return fmt.Errorf("runner log health endpoint is empty")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	delay := initial
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			resp, requestErr := client.Do(req)
			if requestErr == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				last = statusError{resp.StatusCode}
				if ClassifyStatus(resp.StatusCode) == Permanent {
					return last
				}
			} else {
				last = requestErr
			}
		} else {
			return err
		}
		if attempt == retries {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < maximum/2 {
			delay *= 2
		} else {
			delay = maximum
		}
	}
	return fmt.Errorf("runner log endpoint unavailable after %d retries: %w", retries, last)
}
