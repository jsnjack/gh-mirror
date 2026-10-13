package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"gh-mirror/internal/commentpolicy"
	"gh-mirror/internal/config"
)

const commentPolicyKey = "comment_policy"

type savedCommentPolicy struct {
	RegistryVersion string                          `json:"registry_version"`
	Repositories    map[string]commentpolicy.Policy `json:"repositories"`
}

// CommentFiltering describes locally applied policies and retained source counts.
type CommentFiltering struct {
	RegistryVersion string                          `json:"registry_version"`
	Generation      string                          `json:"generation"`
	Policies        map[string]commentpolicy.Policy `json:"policies"`
	Excluded        int                             `json:"excluded"`
}

func commentPolicies(ctx context.Context, q querier) (savedCommentPolicy, error) {
	var saved savedCommentPolicy
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", commentPolicyKey).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return saved, nil
		}
		return saved, fmt.Errorf("read saved comment policy: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return saved, fmt.Errorf("decode saved comment policy: %w", err)
	}
	return saved, nil
}

func prCommentInventory(ctx context.Context, q querier) ([]commentpolicy.Comment, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.id,c.repo,c.number,c.body,COALESCE(json_extract(c.payload,'$.user.login'),''),COALESCE(json_extract(c.payload,'$.user.type'),''),COALESCE(CAST(json_extract(c.payload,'$.user.id') AS TEXT),''),c.url,COALESCE(CAST(json_extract(c.payload,'$.in_reply_to_id') AS TEXT),'') FROM comments c JOIN issues i ON i.repo=c.repo AND i.number=c.number WHERE i.kind='pull_request' ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("read PR comment identities: %w", err)
	}
	out := []commentpolicy.Comment{}
	for rows.Next() {
		var c commentpolicy.Comment
		var source, parent string
		if err := rows.Scan(&c.ID, &c.Repo, &c.Number, &c.Body, &c.Author.Login, &c.Author.Type, &c.Author.ID, &source, &parent); err != nil {
			return nil, finishRows(rows, err)
		}
		u, err := url.Parse(source)
		if err != nil {
			return nil, finishRows(rows, fmt.Errorf("read comment source host: %w", err))
		}
		c.Author.Host = u.Hostname()
		if strings.HasPrefix(c.ID, "review:") && parent != "" {
			c.Parent = "review:" + parent
		}
		out = append(out, c)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyCommentPolicies reconciles derived search documents without changing source comments.
func (w *Writer) ApplyCommentPolicies(ctx context.Context, policies map[string]commentpolicy.Policy) error {
	if policies == nil {
		saved, err := commentPolicies(ctx, w.tx)
		if err != nil {
			return err
		}
		policies = saved.Repositories
		if policies == nil {
			return nil
		}
	}
	for repo, p := range policies {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("comment policy for %s: %w", repo, err)
		}
	}
	comments, err := prCommentInventory(ctx, w.tx)
	if err != nil {
		return err
	}
	excluded, err := commentpolicy.Select(comments, policies)
	if err != nil {
		return fmt.Errorf("resolve PR comment threads: %w", err)
	}
	rows, err := w.tx.QueryContext(ctx, `SELECT d.id FROM documents d JOIN issues i ON i.repo=d.repo AND i.number=d.number WHERE i.kind='pull_request' AND d.id LIKE 'comment:%'`)
	if err != nil {
		return fmt.Errorf("read indexed PR comment identities: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return finishRows(rows, err)
		}
		present[strings.TrimPrefix(id, "comment:")] = true
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	changed := false
	for _, c := range comments {
		_, omit := excluded[c.ID]
		if omit && present[c.ID] {
			if _, err := w.tx.ExecContext(ctx, "DELETE FROM documents WHERE id=?", "comment:"+c.ID); err != nil {
				return fmt.Errorf("exclude comment search document: %w", err)
			}
			changed = true
		} else if !omit && !present[c.ID] {
			if _, err := w.tx.ExecContext(ctx, `INSERT INTO documents(id,repo,number,source,title,body) SELECT 'comment:'||id,repo,number,url,'',body FROM comments WHERE id=?`, c.ID); err != nil {
				return fmt.Errorf("restore comment search document: %w", err)
			}
			changed = true
		}
	}
	saved := savedCommentPolicy{RegistryVersion: commentpolicy.RegistryVersion, Repositories: policies}
	raw, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("encode applied comment policy: %w", err)
	}
	previous, err := w.Metadata(ctx, commentPolicyKey)
	if err != nil {
		return err
	}
	if previous != string(raw) || changed {
		if err := w.SetMetadata(ctx, commentPolicyKey, string(raw)); err != nil {
			return err
		}
		if err := w.SetMetadata(ctx, "comment_policy_generation", hash(raw)); err != nil {
			return err
		}
		if err := advanceSemantic(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

func visibleComment(alias string) string {
	return "EXISTS (SELECT 1 FROM documents visible WHERE visible.id='comment:'||" + alias + ".id)"
}

func excludedCommentCount(ctx context.Context, q querier, repo string, number int) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM comments c WHERE c.repo=? AND c.number=? AND NOT "+visibleComment("c"), repo, number).Scan(&count); err != nil {
		return 0, fmt.Errorf("count excluded comments: %w", err)
	}
	return count, nil
}

func retrievalGeneration(s Status) string {
	if s.CommentFiltering == nil {
		return s.Generation
	}
	return s.Generation + ":" + s.CommentFiltering.Generation
}

func commentFilterWarning(repo string, count int) Warning {
	return Warning{Code: "comments_excluded", Repo: repo, Resource: "comments", Message: fmt.Sprintf("%d comments excluded by the applied PR comment policy; use include_excluded=true (CLI --include-excluded) or get --comments all to inspect retained sources.", count)}
}

// CommentActor reports detected automation and configured author exclusions in one repository.
type CommentActor struct {
	Detection string `json:"detection,omitempty"`
	Repo      string `json:"repo"`
	Login     string `json:"login"`
	Type      string `json:"type"`
	AccountID string `json:"account_id"`
	Provider  string `json:"provider,omitempty"`
	Comments  int    `json:"comments"`
	Excluded  int    `json:"excluded"`
}

// CommentActors reads current account metadata without contacting GitHub.
func (s *Store) CommentActors(ctx context.Context, repo string) ([]CommentActor, error) {
	out := []CommentActor{}
	if repo != "" && !config.ValidRepository(repo) {
		return out, invalid("invalid repository")
	}
	err := s.view(ctx, func(q *sql.Tx) error {
		inventory, err := prCommentInventory(ctx, q)
		if err != nil {
			return err
		}
		saved, err := commentPolicies(ctx, q)
		if err != nil {
			return err
		}
		excluded, err := commentpolicy.Select(inventory, saved.Repositories)
		if err != nil {
			return fmt.Errorf("inspect comment policy: %w", err)
		}
		rows, err := q.QueryContext(ctx, "SELECT c.id FROM comments c JOIN issues i ON i.repo=c.repo AND i.number=c.number WHERE i.kind='pull_request' AND NOT "+visibleComment("c"))
		if err != nil {
			return fmt.Errorf("read actual comment exclusions: %w", err)
		}
		actual := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return finishRows(rows, err)
			}
			actual[id] = true
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
		indices := map[string]int{}
		for _, c := range inventory {
			if repo != "" && repo != c.Repo {
				continue
			}
			provider := commentpolicy.DetectedAI(c)
			detection := "account"
			if commentpolicy.KnownAI(c.Author) == "" && provider != "" {
				detection = "attribution"
			}
			decision, omit := excluded[c.ID]
			if c.Author.Type != "Bot" && provider == "" && (!omit || decision.Reason == "excluded_thread") {
				continue
			}
			if provider == "" && c.Author.Type != "Bot" {
				detection = "configured_author"
			}
			key := c.Repo + "\x00" + c.Author.Login + "\x00" + c.Author.ID + "\x00" + provider + "\x00" + detection
			i, ok := indices[key]
			if !ok {
				i = len(out)
				indices[key] = i
				out = append(out, CommentActor{Repo: c.Repo, Login: c.Author.Login, Type: c.Author.Type, AccountID: c.Author.ID, Provider: provider, Detection: detection})
			}
			out[i].Comments++
			if actual[c.ID] {
				out[i].Excluded++
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		if out[i].Login != out[j].Login {
			return out[i].Login < out[j].Login
		}
		if out[i].AccountID != out[j].AccountID {
			return out[i].AccountID < out[j].AccountID
		}
		return out[i].Provider < out[j].Provider
	})
	return out, err
}

func searchCommentWarning(ctx context.Context, q querier, s Status, f filters, in []string) ([]Warning, error) {
	if s.CommentFiltering == nil || s.CommentFiltering.Excluded == 0 {
		return nil, nil
	}
	if f.Kind == "issue" {
		return nil, nil
	}
	if len(in) > 0 && !slices.Contains(in, "comments") && !slices.Contains(in, "reviews") {
		return nil, nil
	}
	where, args, err := f.sql()
	if err != nil {
		return nil, err
	}
	var count int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM comments c JOIN issues i ON i.repo=c.repo AND i.number=c.number WHERE i.kind='pull_request' AND ("+where+") AND NOT "+visibleComment("c"), args...).Scan(&count); err != nil {
		return nil, fmt.Errorf("count excluded comments in search scope: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	return []Warning{commentFilterWarning(f.Repo, count)}, nil
}
