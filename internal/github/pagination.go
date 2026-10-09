package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"gh-mirror/internal/progress"
)

var errPaginationChanged = errors.New("GitHub numbered pagination changed during collection")

type listingPage struct {
	items      []json.RawMessage
	next, last string
}

// List fetches all pages, parallelizing numbered ranges advertised by GitHub.
func (c *Client) List(ctx context.Context, path string) ([]json.RawMessage, error) {
	return c.ListWithProgress(ctx, path, progress.Event{})
}

// ListWithProgress reports completed pages independently for each resource listing.
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
		page, err := c.fetchListingPage(ctx, target, base, apiPrefix)
		if err != nil {
			return nil, fmt.Errorf("list %s page %d: %w", base.Path, len(seen), err)
		}
		out = append(out, page.items...)
		activity.Page, activity.Records = len(seen), len(out)
		c.Report(activity)
		plan, ok := numberedRange(target, page.next, page.last)
		if !ok || cap(c.slots) == 1 {
			target = page.next
			continue
		}
		pages, err := c.fetchNumberedPages(ctx, plan, base, apiPrefix, activity)
		if err != nil {
			if errors.Is(err, errPaginationChanged) {
				err = c.discardListingPage(ctx, target, err)
			}
			return nil, fmt.Errorf("list %s numbered pages: %w", base.Path, err)
		}
		for index, result := range pages {
			pageURL := plan.url(plan.first + index)
			if seen[pageURL] {
				return nil, fmt.Errorf("repeated GitHub pagination URL")
			}
			seen[pageURL] = true
			out = append(out, result.items...)
		}
		target = pages[len(pages)-1].next
	}
	return out, nil
}

func (c *Client) fetchListingPage(ctx context.Context, target string, base *url.URL, prefix string) (listingPage, error) {
	pageBase, err := url.Parse(target)
	if err != nil {
		return listingPage{}, fmt.Errorf("parse listing page URL: %w", err)
	}
	response, err := c.get(ctx, target)
	if err != nil {
		return listingPage{}, fmt.Errorf("fetch listing page: %w", err)
	}
	var page listingPage
	if err := json.Unmarshal(response.Body, &page.items); err != nil || page.items == nil {
		return page, c.discardListingPage(ctx, target, fmt.Errorf("expected GitHub array at %s", base.Path))
	}
	page.next, page.last, err = listingLinks(response.Link, pageBase, prefix)
	if err != nil {
		return page, c.discardListingPage(ctx, target, err)
	}
	return page, nil
}

func (c *Client) discardListingPage(ctx context.Context, target string, cause error) error {
	if c.Checkpoint != nil {
		if err := c.Checkpoint.Delete(ctx, "GET:"+target); err != nil {
			return errors.Join(cause, fmt.Errorf("discard invalid listing checkpoint: %w", err))
		}
	}
	return cause
}

func listingLinks(header string, base *url.URL, prefix string) (next, last string, err error) {
	for _, link := range strings.Split(header, ",") {
		segments := strings.Split(strings.TrimSpace(link), ";")
		for _, segment := range segments[1:] {
			relation := strings.TrimSpace(segment)
			if relation != `rel="next"` && relation != `rel="last"` {
				continue
			}
			candidate, parseErr := url.Parse(strings.Trim(strings.TrimSpace(segments[0]), "<>"))
			if parseErr != nil {
				return "", "", fmt.Errorf("parse pagination link: %w", parseErr)
			}
			candidate = base.ResolveReference(candidate)
			// GitHub switches /repos/owner/name to /repositories/id in page links.
			if candidate.Scheme != base.Scheme || candidate.Host != base.Host || !strings.HasPrefix(candidate.Path, prefix) || candidate.User != nil || candidate.Fragment != "" {
				return "", "", fmt.Errorf("unsafe GitHub pagination URL")
			}
			destination := &next
			if relation == `rel="last"` {
				destination = &last
			}
			if *destination != "" {
				return "", "", fmt.Errorf("multiple GitHub pagination links for %s", relation)
			}
			*destination = candidate.String()
		}
	}
	return next, last, nil
}

type pageRange struct {
	template    *url.URL
	first, last int
}

func numberedRange(current, next, last string) (pageRange, bool) {
	if next == "" || last == "" {
		return pageRange{}, false
	}
	a, aErr := url.Parse(next)
	b, bErr := url.Parse(last)
	origin, originErr := url.Parse(current)
	if aErr != nil || bErr != nil || originErr != nil || !samePageSeries(a, b) {
		return pageRange{}, false
	}
	first, firstOK := pageNumber(a)
	end, endOK := pageNumber(b)
	prior := 1
	if origin.Query().Has("page") {
		var ok bool
		prior, ok = pageNumber(origin)
		if !ok {
			return pageRange{}, false
		}
	}
	q := origin.Query()
	q.Del("page")
	nq := a.Query()
	nq.Del("page")
	if !firstOK || !endOK || first <= prior || first-prior != 1 || end < first || q.Encode() != nq.Encode() {
		return pageRange{}, false
	}
	return pageRange{a, first, end}, true
}

func pageNumber(u *url.URL) (int, bool) {
	q := u.Query()
	for _, cursor := range []string{"before", "after", "cursor"} {
		if q.Has(cursor) {
			return 0, false
		}
	}
	values := q["page"]
	if len(values) != 1 {
		return 0, false
	}
	n, err := strconv.Atoi(values[0])
	return n, err == nil && n > 0 && n < int(^uint(0)>>1) && strconv.Itoa(n) == values[0] && strings.Contains("&"+u.RawQuery, "&page=")
}

func samePageSeries(a, b *url.URL) bool {
	aq, bq := a.Query(), b.Query()
	aq.Del("page")
	bq.Del("page")
	return a.Scheme == b.Scheme && a.Host == b.Host && a.Path == b.Path && aq.Encode() == bq.Encode()
}

func (p pageRange) url(number int) string {
	u := *p.template
	parts := strings.Split(u.RawQuery, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		if key == "page" {
			// Preserve Link query ordering so older exact-URL checkpoints remain reusable.
			parts[i] = key + "=" + strconv.Itoa(number)
		}
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}

func (c *Client) fetchNumberedPages(ctx context.Context, plan pageRange, base *url.URL, prefix string, activity progress.Event) ([]listingPage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	next := plan.first
	pages := map[int]listingPage{}
	var firstErr error
	var workers sync.WaitGroup
	for range min(cap(c.slots), plan.last-plan.first+1) {
		workers.Go(func() {
			for {
				mu.Lock()
				if next > plan.last || firstErr != nil || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				number := next
				next++
				mu.Unlock()
				target := plan.url(number)
				page, err := c.fetchListingPage(ctx, target, base, prefix)
				if err == nil {
					u, parseErr := url.Parse(page.next)
					validNext := page.next == "" && number == plan.last
					if page.next != "" && parseErr == nil {
						n, ok := pageNumber(u)
						validNext = ok && n-number == 1 && samePageSeries(plan.template, u)
					}
					if !validNext {
						err = c.discardListingPage(ctx, target, fmt.Errorf("%w at page %d", errPaginationChanged, number))
					}
				}
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("fetch page %d: %w", number, err)
						cancel()
					}
					mu.Unlock()
					return
				}
				pages[number] = page
				activity.Page++
				activity.Records += len(page.items)
				c.Report(activity)
				mu.Unlock()
			}
		})
	}
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cancel numbered pagination: %w", err)
	}
	out := make([]listingPage, 0, len(pages))
	for number := plan.first; number <= plan.last; number++ {
		out = append(out, pages[number])
	}
	return out, nil
}
