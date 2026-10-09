// Package github implements bounded, read-only GitHub REST and GraphQL requests.
package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/progress"
)

const maxBody = 32 << 20

// Cache stores conditional responses inside the collection transaction.
type Cache interface {
	Cached(context.Context, string) (string, []byte, error)
	Cache(context.Context, string, string, []byte) error
}

// Checkpoint retains successful fetches independently of the mirror transaction.
type Checkpoint interface {
	Load(context.Context, string) ([]byte, bool, error)
	Save(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

// Client shares a request budget across REST, GraphQL, and retries.
type Client struct {
	Progress              progress.Reporter
	Checkpoint            Checkpoint
	resumed               int
	api, graph, token     string
	http                  *http.Client
	cache                 Cache
	remaining, used       int
	mu, reportMu, cacheMu sync.Mutex
	slots                 chan struct{}
	active                int
	pausedUntil           time.Time
	fatal                 error
}

// New creates a client that refuses redirects and bounds individual requests.
func New(api, graph, token string, budget, workers int, cache Cache) *Client {
	return &Client{api: strings.TrimRight(api, "/"), graph: graph, token: token, remaining: budget, slots: make(chan struct{}, max(1, workers)), cache: cache, http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Requests reports actual HTTP attempts including retries and conditional requests.
func (c *Client) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

type cachedResponse struct {
	Body json.RawMessage `json:"body"`
	Link string          `json:"link"`
	ETag string          `json:"etag,omitempty"`
}

func (c *Client) request(ctx context.Context, method, target string, payload []byte, etag string) ([]byte, http.Header, int, error) {
	for attempt := 0; attempt < 3; attempt++ {
		body, headers, code, err := c.attempt(ctx, method, target, payload, etag, attempt)
		if err != nil || code == http.StatusOK || code == http.StatusNotModified {
			return body, headers, code, err
		}
	}
	return nil, nil, 0, fmt.Errorf("GitHub retries exhausted")
}
func (c *Client) attempt(ctx context.Context, method, target string, payload []byte, etag string, attempt int) (body []byte, headers http.Header, code int, requestErr error) {
	number, err := c.acquire(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() {
		if requestErr != nil && ctx.Err() == nil {
			c.fail(requestErr)
		}
		c.release()
	}()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("prepare GitHub request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "gh-mirror")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("GitHub %s request: %w", method, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, nil, 0, fmt.Errorf("read GitHub response: %w", readErr)
	}
	if closeErr != nil {
		return nil, nil, 0, fmt.Errorf("close GitHub response: %w", closeErr)
	}
	if len(body) > maxBody {
		return nil, nil, 0, fmt.Errorf("GitHub response exceeds %d bytes", maxBody)
	}
	slog.DebugContext(ctx, "GitHub response", "method", method, "status", resp.StatusCode, "request", number, "elapsed", time.Since(started))
	c.Report(progress.Event{Remaining: resp.Header.Get("X-RateLimit-Remaining"), Cached: resp.StatusCode == http.StatusNotModified})
	slog.Log(ctx, diagnostics.TraceLevel, "GitHub request metadata", "path", req.URL.Path, "bytes", len(body), "remaining", resp.Header.Get("X-RateLimit-Remaining"))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified {
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			wait, err := retryDelay(resp.Header, 0)
			if err != nil {
				c.fail(err)
			} else {
				c.pause(wait)
			}
		}
		return body, resp.Header, resp.StatusCode, nil
	}
	retry := resp.StatusCode == 429 || resp.StatusCode >= 500 || (resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if !retry || attempt == 2 {
		return nil, nil, resp.StatusCode, fmt.Errorf("GitHub %s %s returned HTTP %d (request id %s)", method, req.URL.Path, resp.StatusCode, resp.Header.Get("X-GitHub-Request-Id"))
	}
	delay := time.Duration(attempt+1) * time.Second
	if resp.StatusCode == 429 || resp.StatusCode == 403 {
		delay = time.Minute
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			delay = 0
		}
	}
	delay, err = retryDelay(resp.Header, delay)
	if err != nil {
		return nil, nil, resp.StatusCode, err
	}
	c.pause(delay)
	return nil, resp.Header, resp.StatusCode, nil
}

func retryDelay(headers http.Header, delay time.Duration) (time.Duration, error) {
	if h := headers.Get("Retry-After"); h != "" {
		seconds, parseErr := strconv.Atoi(h)
		if parseErr == nil {
			delay = time.Duration(seconds) * time.Second
		} else if date, dateErr := http.ParseTime(h); dateErr == nil {
			delay = time.Until(date)
		} else {
			return 0, fmt.Errorf("invalid GitHub Retry-After header")
		}
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		reset, parseErr := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("invalid GitHub rate reset: %w", parseErr)
		}
		if wait := time.Until(time.Unix(reset, 0)) + time.Second; wait > delay {
			delay = wait
		}
	}
	if delay > 60*time.Second {
		return 0, fmt.Errorf("GitHub rate limit requires a wait exceeding 60 seconds; retry collection later")
	}
	if delay < 0 {
		delay = 0
	}
	return delay, nil
}
func (c *Client) get(ctx context.Context, target string) (cachedResponse, error) {
	var cached cachedResponse
	key := "GET:" + target
	saved, found, err := c.load(ctx, key)
	if err != nil {
		return cached, err
	}
	if found {
		if err := json.Unmarshal(saved, &cached); err != nil {
			return cached, fmt.Errorf("decode saved REST response: %w", err)
		}
		return cached, c.remember(ctx, target, cached, saved)
	}
	etag := ""
	u, err := url.Parse(target)
	if err != nil {
		return cached, fmt.Errorf("parse request URL: %w", err)
	}
	// Each since watermark changes the URL; persisting those responses grows without reuse.
	cacheable := c.cache != nil && !u.Query().Has("since")
	if cacheable {
		var body []byte
		var err error
		c.cacheMu.Lock()
		etag, body, err = c.cache.Cached(ctx, target)
		c.cacheMu.Unlock()
		if err != nil {
			return cached, fmt.Errorf("load conditional response: %w", err)
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &cached); err != nil {
				return cached, fmt.Errorf("decode conditional response: %w", err)
			}
		}
	}
	body, headers, code, err := c.request(ctx, http.MethodGet, target, nil, etag)
	if err != nil {
		return cached, fmt.Errorf("fetch GitHub data: %w", err)
	}
	if code == http.StatusNotModified {
		if len(cached.Body) == 0 {
			return cached, fmt.Errorf("GitHub returned 304 without cached response")
		}
		encoded, err := json.Marshal(cached)
		if err != nil {
			return cached, fmt.Errorf("encode saved conditional response: %w", err)
		}
		if err := c.save(ctx, key, encoded); err != nil {
			return cached, err
		}
		return cached, nil
	}
	if !json.Valid(body) {
		return cached, fmt.Errorf("GitHub returned invalid JSON")
	}
	out := cachedResponse{Body: body, Link: headers.Get("Link"), ETag: headers.Get("ETag")}
	encoded, err := json.Marshal(out)
	if err != nil {
		return out, fmt.Errorf("encode REST response: %w", err)
	}
	if err := c.save(ctx, key, encoded); err != nil {
		return out, err
	}
	return out, c.remember(ctx, target, out, encoded)
}

func (c *Client) remember(ctx context.Context, target string, response cachedResponse, encoded []byte) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse response URL: %w", err)
	}
	if c.cache != nil && response.ETag != "" && !u.Query().Has("since") {
		c.cacheMu.Lock()
		err = c.cache.Cache(ctx, target, response.ETag, encoded)
		c.cacheMu.Unlock()
		if err != nil {
			return fmt.Errorf("cache conditional response: %w", err)
		}
	}
	return nil
}

// Get fetches one raw REST object relative to the configured API base.
func (c *Client) Get(ctx context.Context, path string) (json.RawMessage, error) {
	out, err := c.get(ctx, c.api+path)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", path, err)
	}
	return out.Body, nil
}

// List follows every REST next link, rejecting cycles and foreign origins.
func (c *Client) List(ctx context.Context, path string) ([]json.RawMessage, error) {
	return c.ListWithProgress(ctx, path, progress.Event{})
}

// ListWithProgress identifies a listing so parallel page counters remain independent.
func (c *Client) ListWithProgress(ctx context.Context, path string, activity progress.Event) ([]json.RawMessage, error) {
	target := c.api + path
	base, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse listing URL: %w", err)
	}
	apiBase, err := url.Parse(c.api)
	if err != nil {
		return nil, fmt.Errorf("parse API base: %w", err)
	}
	apiPrefix := strings.TrimRight(apiBase.Path, "/") + "/"
	seen := map[string]bool{}
	out := []json.RawMessage{}
	for target != "" {
		if seen[target] {
			return nil, fmt.Errorf("repeated GitHub pagination URL")
		}
		seen[target] = true
		page, err := c.get(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("list %s page %d: %w", base.Path, len(seen), err)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(page.Body, &items); err != nil || items == nil {
			return nil, fmt.Errorf("expected GitHub array at %s", base.Path)
		}
		out = append(out, items...)
		activity.Page, activity.Records = len(seen), len(out)
		c.Report(activity)
		target = ""
		for _, link := range strings.Split(page.Link, ",") {
			segments := strings.Split(strings.TrimSpace(link), ";")
			if len(segments) < 2 {
				continue
			}
			next := false
			for _, segment := range segments[1:] {
				if strings.TrimSpace(segment) == `rel="next"` {
					next = true
				}
			}
			if !next {
				continue
			}
			candidate, err := url.Parse(strings.Trim(strings.TrimSpace(segments[0]), "<>"))
			if err != nil {
				return nil, fmt.Errorf("parse next page: %w", err)
			}
			candidate = base.ResolveReference(candidate)
			// GitHub may switch /repos/owner/name to /repositories/id in next links.
			if candidate.Scheme != base.Scheme || candidate.Host != base.Host || !strings.HasPrefix(candidate.Path, apiPrefix) || candidate.User != nil || candidate.Fragment != "" {
				return nil, fmt.Errorf("unsafe GitHub pagination URL")
			}
			if target != "" {
				return nil, fmt.Errorf("multiple GitHub next links")
			}
			target = candidate.String()
		}
	}
	return out, nil
}

// GraphQL executes a query document and rejects all partial-error responses.
func (c *Client) GraphQL(ctx context.Context, query string, variables any, out any) error {
	return c.GraphQLValidated(ctx, query, variables, out, nil)
}

// GraphQLValidated validates observations before checkpointing and refetches invalid legacy saves.
func (c *Client) GraphQLValidated(ctx context.Context, query string, variables any, out any, validate func(json.RawMessage) error) error {
	if !strings.HasPrefix(strings.TrimSpace(query), "query") {
		return fmt.Errorf("only GraphQL query documents are supported")
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encode GraphQL query: %w", err)
	}
	key := fmt.Sprintf("GraphQL:%x", sha256.Sum256(append([]byte(c.graph+"\x00"), payload...)))
	saved, found, err := c.load(ctx, key)
	if err != nil {
		return err
	}
	if found {
		err := json.Unmarshal(saved, out)
		if err == nil && validate != nil {
			err = validate(saved)
		}
		if err == nil {
			return nil
		}
		if err := c.Checkpoint.Delete(ctx, key); err != nil {
			return fmt.Errorf("discard invalid GraphQL observation: %w", err)
		}
	}
	body, _, _, err := c.request(ctx, http.MethodPost, c.graph, payload, "")
	if err != nil {
		return fmt.Errorf("fetch GraphQL data: %w", err)
	}
	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode GraphQL envelope: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL returned %d errors; verify feature support and token permissions", len(envelope.Errors))
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("GitHub GraphQL returned no data")
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode GraphQL data: %w", err)
	}
	if validate != nil {
		if err := validate(envelope.Data); err != nil {
			return fmt.Errorf("validate GraphQL observations: %w", err)
		}
	}
	return c.save(ctx, key, envelope.Data)
}
