package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// fireCallback POSTs the finished run as JSON to url. Best-effort, one retry.
func (r *Runner) fireCallback(url string, run model.Run) {
	body, err := json.Marshal(run)
	if err != nil {
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(3 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				r.log.Info("run callback delivered", "run", run.ID, "status", resp.StatusCode)
				return
			}
		}
		r.log.Warn("run callback failed", "run", run.ID, "attempt", attempt+1, "err", err)
	}
}
