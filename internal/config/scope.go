package config

// Scope selects upstream resource families for one repository. Omitted flags in an explicit scope are false.
type Scope struct {
	Issues                    bool `json:"issues"`
	PullRequests              bool `json:"pull_requests"`
	IssueComments             bool `json:"issue_comments"`
	PullRequestComments       bool `json:"pull_request_comments"`
	PullRequestReviewComments bool `json:"pull_request_review_comments"`
	Labels                    bool `json:"labels"`
	Milestones                bool `json:"milestones"`
	IssueTypes                bool `json:"issue_types"`
	Fields                    bool `json:"fields"`
	Relationships             bool `json:"relationships"`
	Projects                  bool `json:"projects"`
}

// DefaultScope preserves collection behavior for repositories without explicit options.
func (c Config) DefaultScope() Scope {
	return Scope{Issues: true, PullRequests: true, IssueComments: true, PullRequestComments: true, Labels: true, Milestones: true, IssueTypes: c.Fields, Fields: c.Fields, Relationships: true, Projects: c.Projects}
}

// Scope resolves a repository's explicit options or the compatible global defaults.
func (c Config) Scope(repo string) Scope {
	scope, ok := c.RepositoryOptions[repo]
	if !ok {
		scope = c.DefaultScope()
	}
	scope.IssueComments = scope.IssueComments && scope.Issues
	scope.PullRequestComments = scope.PullRequestComments && scope.PullRequests
	scope.PullRequestReviewComments = scope.PullRequestReviewComments && scope.PullRequests
	scope.Fields = scope.Fields && scope.Issues
	scope.IssueTypes = scope.IssueTypes && scope.Issues
	scope.Relationships = scope.Relationships && scope.Issues
	return scope
}

// Scopes returns resolved options for each configured repository.
func (c Config) Scopes() map[string]Scope {
	out := make(map[string]Scope, len(c.Repositories))
	for _, repo := range c.Repositories {
		out[repo] = c.Scope(repo)
	}
	return out
}

// Includes reports whether the scope retains a ticket's resource kind.
func (s Scope) Includes(kind string) bool {
	return (kind == "issue" && s.Issues) || (kind == "pull_request" && s.PullRequests)
}

// Comments reports whether conversation comments are retained for this ticket kind.
func (s Scope) Comments(kind string) bool {
	return (kind == "issue" && s.IssueComments) || (kind == "pull_request" && s.PullRequestComments)
}
