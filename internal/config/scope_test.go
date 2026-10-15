package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryOptions(t *testing.T) {
	for _, name := range []string{"legacy defaults", "global defaults", "explicit all", "PR only", "empty scope", "unknown option", "invalid boolean"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := `[repos."o/r"]`
			switch name {
			case "global defaults":
				body = "fields = false\nprojects = false\n" + body
			case "explicit all":
				body += "\npreset = 'all'\npull_request_review_comments = false"
			case "PR only":
				body += "\npreset = 'pull_requests'\npull_request_review_comments = false"
			case "empty scope":
				body += "\nissues = false\npull_requests = false"
			case "unknown option":
				body += "\nisssues = true"
			case "invalid boolean":
				body += "\nissues = 'true'"
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path, true)
			if name == "unknown option" || name == "invalid boolean" {
				if err == nil {
					t.Fatal("accepted invalid options")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = c.Validate()
			if name == "empty scope" {
				if err == nil {
					t.Fatal("accepted invalid repository scope")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			scope := c.Scope("o/r")
			if c.Scopes()["o/r"] != scope {
				t.Fatal("resolved options disagree")
			}
			if name == "PR only" {
				if scope.Issues || scope.IssueComments || scope.Labels || scope.Fields || scope.Relationships || scope.Projects || !scope.Includes("pull_request") || !scope.Comments("pull_request") {
					t.Fatal("PR scope inherited unrelated resources", scope)
				}
			} else if scope != c.DefaultScope() {
				t.Fatal("legacy scope changed", scope)
			}
		})
	}
}
