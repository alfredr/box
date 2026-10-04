package box

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var notifyClient = &http.Client{Timeout: 10 * time.Second}

// Notify logs a deployment notice and posts it to cfg.Notify when configured. Delivery failures
// are logged and do not fail the calling operation.
func Notify(cfg Config, title, body string, urgent bool) {
	slog.Info(title, "detail", body)
	if cfg.Notify == "" {
		return
	}

	if err := SendNotice(cfg.Notify, "box: "+title, body, urgent); err != nil {
		slog.Warn("notifying", "url", cfg.Notify, "err", err)
	}
}

// SendNotice posts a UTF-8 plain-text body with an ntfy Title header. Urgent notices also set
// high priority and a warning tag. Requests have a ten-second timeout, and responses with
// status 300 or higher return an error.
func SendNotice(url, title, body string, urgent bool) error {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	if urgent {
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "warning")
	}

	resp, err := notifyClient.Do(req)
	if err != nil {
		return err
	}

	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &statusError{resp.Status}
	}

	return nil
}

type statusError struct{ status string }

// Error describes the HTTP status returned by the notification endpoint.
func (e *statusError) Error() string { return "server answered " + e.status }
