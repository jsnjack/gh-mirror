package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryOptions(t *testing.T) {
	for _, name := range []string{"legacy defaults", "global defaults", "explicit all", "PR only", "unknown repo", "empty scope", "unknown option", "invalid boolean"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			body := `{"repositories":["o/r"]}`
			switch name {
			case "global defaults":
				body = `{"repositories":["o/r"],"fields":false,"projects":false}`
			case "explicit all":
				body = `{"repositories":["o/r"],"repository_options":{"o/r":{"issues":true,"pull_requests":true,"issue_comments":true,"pull_request_comments":true,"labels":true,"milestones":true,"issue_types":true,"fields":true,"relationships":true,"projects":true}}}`
			case "PR only":
				body = `{"repositories":["o/r"],"repository_options":{"o/r":{"pull_requests":true,"pull_request_comments":true}}}`
			case "unknown repo":
				body = `{"repositories":["o/r"],"repository_options":{"o/other":{"issues":true}}}`
			case "empty scope":
				body = `{"repositories":["o/r"],"repository_options":{"o/r":{}}}`
			case "unknown option":
				body = `{"repositories":["o/r"],"repository_options":{"o/r":{"isssues":true}}}`
			case "invalid boolean":
				body = `{"repositories":["o/r"],"repository_options":{"o/r":{"issues":"true"}}}`
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
			if name == "unknown repo" || name == "empty scope" {
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
