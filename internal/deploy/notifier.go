package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type HTTPNotifier struct {
	url    string
	key    string
	client *http.Client
}

func NewHTTPNotifier(url, token string) *HTTPNotifier {
	return &HTTPNotifier{
		url: strings.TrimSpace(url), key: strings.TrimSpace(token),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *HTTPNotifier) Notify(ctx context.Context, message string) error {
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.key != "" {
		req.Header.Set("X-API-Key", n.key)
	}
	res, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("notification service returned HTTP %d", res.StatusCode)
	}
	return nil
}
