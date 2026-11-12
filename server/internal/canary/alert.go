package canary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ntfy is where alerts go.
type Ntfy struct {
	URL, Topic, Token string
	Client            *http.Client
}

// Alert posts one notification for the results that did not pass. It sends nothing when all passed.
// Failures are priority 4, slow-only runs 3.
func (n Ntfy) Alert(ctx context.Context, results []Result) error {
	var lines []string
	priority, failed := 3, 0
	for _, r := range results {
		if r.OK() {
			continue
		}
		if r.Err != nil {
			priority = 4
			failed++
			lines = append(lines, fmt.Sprintf("%s: %s failed: %v", r.Target, r.Step, r.Err))
		} else {
			lines = append(lines, fmt.Sprintf("%s: reply took %s", r.Target, r.Elapsed.Round(time.Second)))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	title := "Mainloop canary slow"
	if failed > 0 {
		title = "Mainloop canary failed"
	}
	msg := strings.Join(lines, "\n")
	if r := []rune(msg); len(r) > 1000 {
		msg = string(r[:999]) + "…"
	}
	body, err := json.Marshal(map[string]any{
		"topic": n.Topic, "title": title, "message": msg, "priority": priority, "tags": []string{"warning"},
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	c := n.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy: status %d", resp.StatusCode)
	}
	return nil
}
