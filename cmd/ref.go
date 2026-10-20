package cmd

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gh-mirror/internal/config"
	"gh-mirror/internal/store"
)

const refHelp = `REF is owner/repo#N, a GitHub issue or pull request URL, or #N/N when
default_repo is configured or exactly one repository is configured.`

// parseTicketRef accepts owner/repo#N, GitHub issue/pull URLs, and #N or N with a default repository.
func parseTicketRef(raw, defaultRepo string) (store.TicketID, error) {
	raw = strings.TrimSpace(raw)
	repo, number := "", ""
	switch {
	case strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://"):
		u, err := url.Parse(raw)
		if err != nil {
			return store.TicketID{}, fmt.Errorf("parse ticket URL %q: %w", raw, err)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull" && parts[2] != "pulls") {
			return store.TicketID{}, fmt.Errorf("ticket URL %q must name /owner/repo/issues/N or /owner/repo/pull/N", raw)
		}
		repo, number = parts[0]+"/"+parts[1], parts[3]
	case strings.Contains(raw, "#") && !strings.HasPrefix(raw, "#"):
		repo, number, _ = strings.Cut(raw, "#")
	default:
		number = strings.TrimPrefix(raw, "#")
		if defaultRepo == "" {
			return store.TicketID{}, fmt.Errorf("ticket %q needs a repository: use owner/repo#N or set default_repo in the configuration", raw)
		}
		repo = defaultRepo
	}
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 {
		return store.TicketID{}, fmt.Errorf("ticket %q needs a positive number", raw)
	}
	if !config.ValidRepository(repo) {
		return store.TicketID{}, fmt.Errorf("ticket %q has an invalid repository %q", raw, repo)
	}
	return store.TicketID{Repo: repo, Number: n}, nil
}

// resolveTicket reads the positional REF.
func resolveTicket(args []string) (store.TicketID, error) {
	if len(args) == 0 {
		return store.TicketID{}, fmt.Errorf("ticket REF is required, such as owner/repo#123")
	}
	return parseTicketRef(args[0], settings.DefaultRepository())
}
