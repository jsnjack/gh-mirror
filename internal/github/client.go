// Package github implements bounded, read-only GitHub REST and GraphQL requests.
package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

const (
	maxServerAttempts = 3
	maxRateAttempts   = 5
	secondaryRateWait = time.Minute
	// Primary rate limits reset within an hour; longer waits indicate a misconfigured upstream.
	maxRetryWait = time.Hour + time.Minute
)

// StatusError reports a final non-success HTTP status without the response body.
type StatusError struct {
	Method, Path, RequestID string
	Code, Attempts          int
}

func (e *StatusError) Error() string {
	if e.Attempts > 0 {
		return fmt.Sprintf("GitHub %s %s returned HTTP %d after %d attempts (request id %s)", e.Method, e.Path, e.Code, e.Attempts, e.RequestID)
	}
	return fmt.Sprintf("GitHub %s %s returned HTTP %d (request id %s)", e.Method, e.Path, e.Code, e.RequestID)
}

// Absent reports statuses GitHub uses for deleted, transferred or inaccessible resources.
func (e *StatusError) Absent() bool {
	return e.Code == http.StatusNotFound || e.Code == http.StatusGone || e.Code == http.StatusMovedPermanently
}

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
	now                   func() time.Time
	sleep                 func(context.Context, time.Duration) error
}

// New creates a client that refuses redirects and bounds individual requests.
func New(api, graph, token string, budget, workers int, cache Cache) *Client {
	return &Client{api: strings.TrimRight(api, "/"), graph: graph, token: token, remaining: budget, slots: make(chan struct{}, max(1, workers)), cache: cache, now: time.Now, sleep: sleep, http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func (c *Client) request(ctx context.Context, method, target string, payload []byte, etag string) (body []byte, headers http.Header, code int, requestErr error) {
	defer func() {
		var status *StatusError
		absent := errors.As(requestErr, &status) && status.Absent()
		if requestErr != nil && ctx.Err() == nil && !absent {
			c.fail(requestErr)
		}
	}()
	for attempt := 1; ; attempt++ {
		response, err := c.attempt(ctx, method, target, payload, etag)
		if err != nil {
			return nil, nil, 0, err
		}
		if response.code == http.StatusOK || response.code == http.StatusNotModified {
			return response.body, response.headers, response.code, nil
		}
		status := &StatusError{Method: method, Path: response.path, Code: response.code, RequestID: response.headers.Get("X-GitHub-Request-Id")}
		var delay time.Duration
		switch {
		case rateLimited(response):
			if attempt >= maxRateAttempts {
				status.Attempts = attempt
				return nil, nil, response.code, status
			}
			delay, err = c.rateWait(response.headers, attempt)
		case response.code >= http.StatusInternalServerError:
			if attempt >= maxServerAttempts {
				status.Attempts = attempt
				return nil, nil, response.code, status
			}
			delay, err = c.retryDelay(response.headers, time.Duration(attempt)*time.Second)
		default:
			return nil, nil, response.code, status
		}
		if err != nil {
			return nil, nil, response.code, err
		}
		c.pause(delay)
	}
}

type response struct {
	body    []byte
	headers http.Header
	code    int
	path    string
}

func (c *Client) attempt(ctx context.Context, method, target string, payload []byte, etag string) (response, error) {
	number, err := c.acquire(ctx)
	if err != nil {
		return response{}, err
	}
	defer c.release()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return response{}, fmt.Errorf("prepare GitHub request: %w", err)
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
		return response{}, fmt.Errorf("GitHub %s request: %w", method, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return response{}, fmt.Errorf("read GitHub response: %w", readErr)
	}
	if closeErr != nil {
		return response{}, fmt.Errorf("close GitHub response: %w", closeErr)
	}
	if len(body) > maxBody {
		return response{}, fmt.Errorf("GitHub response exceeds %d bytes", maxBody)
	}
	slog.DebugContext(ctx, "GitHub response", "method", method, "status", resp.StatusCode, "request", number, "elapsed", time.Since(started))
	c.Report(progress.Event{Remaining: resp.Header.Get("X-RateLimit-Remaining"), Cached: resp.StatusCode == http.StatusNotModified})
	slog.Log(ctx, diagnostics.TraceLevel, "GitHub request metadata", "path", req.URL.Path, "bytes", len(body), "remaining", resp.Header.Get("X-RateLimit-Remaining"))
	if (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified) && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		wait, err := c.retryDelay(resp.Header, 0)
		if err != nil {
			c.fail(err)
		} else {
			c.pause(wait)
		}
	}
	return response{body: body, headers: resp.Header, code: resp.StatusCode, path: req.URL.Path}, nil
}

func rateLimited(r response) bool {
	if r.code == http.StatusTooManyRequests {
		return true
	}
	if r.code != http.StatusForbidden {
		return false
	}
	// Secondary limits may arrive as 403 without Retry-After while primary quota remains.
	return r.headers.Get("Retry-After") != "" || r.headers.Get("X-RateLimit-Remaining") == "0" || strings.Contains(strings.ToLower(string(r.body)), "secondary rate limit")
}

func (c *Client) rateWait(headers http.Header, attempt int) (time.Duration, error) {
	// GitHub asks clients to wait at least a minute, then back off, when it omits Retry-After.
	fallback := secondaryRateWait << (attempt - 1)
	if headers.Get("X-RateLimit-Remaining") == "0" {
		fallback = 0
	}
	return c.retryDelay(headers, fallback)
}

func (c *Client) retryDelay(headers http.Header, delay time.Duration) (time.Duration, error) {
	if h := headers.Get("Retry-After"); h != "" {
		seconds, parseErr := strconv.Atoi(h)
		if parseErr == nil {
			delay = time.Duration(seconds) * time.Second
		} else if date, dateErr := http.ParseTime(h); dateErr == nil {
			delay = date.Sub(c.now())
		} else {
			return 0, fmt.Errorf("invalid GitHub Retry-After header")
		}
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		reset, parseErr := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("invalid GitHub rate reset: %w", parseErr)
		}
		if wait := time.Unix(reset, 0).Sub(c.now()) + time.Second; wait > delay {
			delay = wait
		}
	}
	if delay > maxRetryWait {
		return 0, fmt.Errorf("GitHub rate limit requires a wait of %s, exceeding %s; retry collection later", delay.Round(time.Second), maxRetryWait)
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
		if links, present := headers["Link"]; present {
			cached.Link = strings.Join(links, ",")
		}
		encoded, err := json.Marshal(cached)
		if err != nil {
			return cached, fmt.Errorf("encode saved conditional response: %w", err)
		}
		if err := c.save(ctx, key, encoded); err != nil {
			return cached, err
		}
		return cached, c.remember(ctx, target, cached, encoded)
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

// Lookup fetches one REST object and reports false when GitHub says it was deleted, transferred or is inaccessible.
func (c *Client) Lookup(ctx context.Context, path string) (json.RawMessage, bool, error) {
	raw, err := c.Get(ctx, path)
	var status *StatusError
	if errors.As(err, &status) && status.Absent() {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
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
	var envelope graphQLEnvelope
	for attempt := 1; ; attempt++ {
		body, headers, _, err := c.request(ctx, http.MethodPost, c.graph, payload, "")
		if err != nil {
			return fmt.Errorf("fetch GraphQL data: %w", err)
		}
		envelope = graphQLEnvelope{}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fmt.Errorf("decode GraphQL envelope: %w", err)
		}
		if !envelope.rateLimited() {
			break
		}
		if attempt >= maxRateAttempts {
			return fmt.Errorf("GitHub GraphQL stayed rate limited after %d attempts: %s", attempt, summarize(envelope.Errors))
		}
		delay, err := c.rateWait(headers, attempt)
		if err != nil {
			return fmt.Errorf("wait for GraphQL rate limit: %w", err)
		}
		c.pause(delay)
	}
	if errs := envelope.unexpected(); len(errs) > 0 {
		return fmt.Errorf("GitHub GraphQL returned %d errors (%s); verify feature support and token permissions", len(errs), summarize(errs))
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

type graphQLError struct {
	Type    string            `json:"type"`
	Message string            `json:"message"`
	Path    []json.RawMessage `json:"path"`
}

type graphQLEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

func (e graphQLEnvelope) rateLimited() bool {
	for _, item := range e.Errors {
		if item.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}

// unexpected omits NOT_FOUND errors for top-level nodes(ids:) entries; callers validate those null nodes.
func (e graphQLEnvelope) unexpected() []graphQLError {
	out := []graphQLError{}
	for _, item := range e.Errors {
		if item.Type == "NOT_FOUND" && len(item.Path) == 2 && string(item.Path[0]) == `"nodes"` {
			continue
		}
		out = append(out, item)
	}
	return out
}

func summarize(errs []graphQLError) string {
	parts := []string{}
	for _, item := range errs[:min(3, len(errs))] {
		text := item.Message
		if runes := []rune(text); len(runes) > 200 {
			text = string(runes[:200]) + "…"
		}
		if item.Type != "" {
			text = item.Type + ": " + text
		}
		parts = append(parts, strconv.Quote(text))
	}
	if len(errs) > 3 {
		parts = append(parts, fmt.Sprintf("%d more", len(errs)-3))
	}
	return strings.Join(parts, "; ")
}
