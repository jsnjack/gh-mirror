// Package progress reports collection activity without exposing upstream payloads.
package progress

import "time"

// FetchingIssues identifies issue pages whose records count toward the sync total.
const FetchingIssues = "Fetching issues"

// FetchingComments identifies discussion comment pages counted toward the sync total.
const FetchingComments = "Fetching comments"

// FetchingReviewComments identifies inline PR review comment pages counted toward the sync total.
const FetchingReviewComments = "Fetching review comments"

// CPUWorkers identifies offline inference activity in collection progress.
const CPUWorkers = "CPU"

// GitHubWorkers identifies upstream request concurrency in collection progress.
const GitHubWorkers = "GitHub"

// Event describes a phase, listing page, completed batch, or HTTP attempt.
type Event struct {
	Phase        string
	Scope        string
	Resource     string
	Page         int
	Records      int
	Completed    int
	Total        int
	Advance      int
	Repository   int
	Repositories int
	Resumed      int
	Saved        int
	Requests     int
	Limit        int
	Remaining    string
	Cached       bool
	Wait         time.Duration
	WaitUntil    time.Time
	Active       int
	Workers      int
	WorkerKind   string
}

// Reporter receives activity synchronously; a nil reporter disables reporting.
type Reporter func(Event)

// Send delivers an event when a reporter is configured.
func (r Reporter) Send(event Event) {
	if r != nil {
		r(event)
	}
}
