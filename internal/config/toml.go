package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

func object(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode configuration object: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode configuration object: %w", err)
	}
	return out, nil
}

func presetScope(name string) (Scope, error) {
	switch name {
	case "all":
		return Scope{Issues: true, PullRequests: true, IssueComments: true, PullRequestComments: true, PullRequestReviewComments: true, Labels: true, Milestones: true, IssueTypes: true, Fields: true, Relationships: true, Projects: true}, nil
	case "issues":
		return Scope{Issues: true, IssueComments: true, Labels: true, Milestones: true, IssueTypes: true, Fields: true, Relationships: true, Projects: true}, nil
	case "pull_requests":
		return Scope{PullRequests: true, PullRequestComments: true, PullRequestReviewComments: true}, nil
	default:
		return Scope{}, fmt.Errorf("unknown repository preset %q; choose all, issues or pull_requests", name)
	}
}

func decodeTOML(r io.Reader, defaults Config) ([]byte, error) {
	var data map[string]any
	if err := toml.NewDecoder(r).Decode(&data); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if value, ok := data["fields"].(bool); ok {
		defaults.Fields = value
	}
	if value, ok := data["projects"].(bool); ok {
		defaults.Projects = value
	}
	if raw, ok := data["repos"]; ok {
		if _, exists := data["repositories"]; exists {
			return nil, fmt.Errorf("use repos tables or legacy repositories, not both")
		}
		if _, exists := data["repository_options"]; exists {
			return nil, fmt.Errorf("repos tables cannot be combined with repository_options")
		}
		repos, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("repos must contain repository tables")
		}
		names := make([]string, 0, len(repos))
		options := map[string]any{}
		policies := map[string]any{}
		for name, raw := range repos {
			entry, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("repository %s must be a table", name)
			}
			if policy, ok := entry["pr_comments"]; ok {
				policies[name] = policy
				delete(entry, "pr_comments")
			}
			base := defaults.DefaultScope()
			if raw, ok := entry["preset"]; ok {
				preset, ok := raw.(string)
				if !ok {
					return nil, fmt.Errorf("repository %s preset must be a string", name)
				}
				var err error
				base, err = presetScope(preset)
				if err != nil {
					return nil, fmt.Errorf("repository %s: %w", name, err)
				}
				delete(entry, "preset")
			}
			fields, err := object(base)
			if err != nil {
				return nil, err
			}
			for key, value := range entry {
				fields[key] = value
			}
			options[name] = fields
			names = append(names, name)
		}
		sort.Strings(names)
		data["repositories"], data["repository_options"] = names, options
		if len(policies) > 0 {
			if _, exists := data["pr_comment_overrides"]; exists {
				return nil, fmt.Errorf("repository pr_comments tables cannot be combined with pr_comment_overrides")
			}
			data["pr_comment_overrides"] = policies
		}
		delete(data, "repos")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("normalize TOML configuration: %w", err)
	}
	return raw, nil
}

// TOML renders compact settings or every resolved setting and repository switch.
func (c Config) TOML(effective bool) ([]byte, error) {
	data, err := object(c)
	if err != nil {
		return nil, err
	}
	if !effective {
		base, err := defaults()
		if err != nil {
			return nil, err
		}
		original, err := object(base)
		if err != nil {
			return nil, err
		}
		for key, value := range data {
			if reflect.DeepEqual(value, original[key]) {
				delete(data, key)
			}
		}
	}
	repos := map[string]any{}
	for _, name := range c.Repositories {
		fields, err := object(c.Scope(name))
		if err != nil {
			return nil, err
		}
		if !effective {
			best := fields
			for _, name := range []string{"all", "issues", "pull_requests"} {
				scope, err := presetScope(name)
				if err != nil {
					return nil, err
				}
				base, err := object(scope)
				if err != nil {
					return nil, err
				}
				entry := map[string]any{"preset": name}
				for key, value := range fields {
					if value != base[key] {
						entry[key] = value
					}
				}
				if len(entry) < len(best) {
					best = entry
				}
			}
			fields = best
		}
		repos[name] = fields
		if effective {
			p := c.CommentPolicies()[name]
			authors, included := append([]string{}, p.ExcludeAuthors...), append([]string{}, p.IncludeAuthors...)
			fields["pr_comments"] = map[string]any{"exclude_known_ai": p.ExcludeKnownAI, "exclude_ai_threads": p.ExcludeAIThreads, "exclude_all_bots": p.ExcludeAllBots, "exclude_authors": authors, "include_authors": included}
		} else if override, ok := c.PRCommentOverrides[name]; ok {
			fields["pr_comments"], err = object(override)
			if err != nil {
				return nil, err
			}
		}
	}
	delete(data, "repositories")
	delete(data, "repository_options")
	delete(data, "pr_comment_overrides")
	if len(repos) > 0 {
		data["repos"] = repos
	}
	raw, err := toml.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("render TOML configuration: %w", err)
	}
	return raw, nil
}

// Migrate writes a private TOML configuration without replacing an existing file.
func Migrate(source, destination string) error {
	c, err := Load(source, true)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("validate migration source: %w", err)
	}
	raw, err := c.TOML(false)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".gh-mirror-config-*.toml")
	if err != nil {
		return fmt.Errorf("create TOML configuration: %w", err)
	}
	defer func() {
		if err := os.Remove(f.Name()); err != nil {
			slog.Debug("remove migration temporary file", "error", err)
		}
	}()
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write TOML configuration: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close TOML configuration: %w", closeErr)
	}
	if err := os.Link(f.Name(), destination); err != nil {
		return fmt.Errorf("install TOML configuration without replacing existing files: %w", err)
	}
	return nil
}
