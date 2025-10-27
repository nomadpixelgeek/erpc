package subsquid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("subsquid: unsupported")

type Client struct {
	baseGateway string
	http        *http.Client
}

func NewClient(gateway string) *Client {
	g := strings.TrimSuffix(strings.TrimPrefix(gateway, "https://"), "/")
	return &Client{
		baseGateway: g,
		http:        &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) routerURL(path string) string {
	return "https://" + c.baseGateway + strings.TrimPrefix(path, "/")
}

func (c *Client) workerPickURL(from uint64) string {
	return fmt.Sprintf("https://%s/%d/worker", c.baseGateway, from)
}

func (c *Client) GetHeight(ctx context.Context) (uint64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.routerURL("/height"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("router height %d: %s", resp.StatusCode, string(b))
	}
	var out RouterHeightResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Height, nil
}

func (c *Client) PickWorker(ctx context.Context, from uint64) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.workerPickURL(from), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("worker pick %d: %s", resp.StatusCode, string(b))
	}
	b, _ := io.ReadAll(resp.Body)
	worker := strings.TrimSpace(string(b))
	if _, err := url.Parse(worker); err != nil {
		return "", fmt.Errorf("invalid worker url: %s", worker)
	}
	return worker, nil
}

func (c *Client) QueryWorker(ctx context.Context, workerURL string, q *WorkerQuery) (*WorkerResponse, error) {
	body, _ := json.Marshal(q)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, workerURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("worker query %d: %s", resp.StatusCode, string(b))
	}
	var out WorkerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func hexU64(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	if strings.HasPrefix(s, "0x") {
		return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	}
	return strconv.ParseUint(s, 10, 64)
}

func u64Hex(n uint64) string { return fmt.Sprintf("0x%x", n) }
