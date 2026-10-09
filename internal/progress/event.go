// Package progress reports collection activity without exposing upstream payloads.
package progress

import "time"

// FetchingIssues identifies issue pages whose records count toward the sync total.
const FetchingIssues = "Fetching issues"

// FetchingComments identifies comment pages whose records count toward the sync total.
const FetchingComments = "Fetching comments"

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
}

// Reporter receives activity synchronously; a nil reporter disables reporting.
type Reporter func(Event)

// Send delivers an event when a reporter is configured.
func (r Reporter) Send(event Event) {
	if r != nil {
		r(event)
	}
}
