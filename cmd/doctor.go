package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gh-mirror/internal/commentpolicy"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

// Doctor check outcomes; only failures make the command exit with an error.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
)

var errDoctorFailed = errors.New("doctor found failing checks")

// doctorCheck is one named health check result.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// doctorReport lists every check and whether all of them passed or only warned.
type doctorReport struct {
	OK     bool          `json:"ok"`
	DBPath string        `json:"db_path,omitempty"`
	Checks []doctorCheck `json:"checks"`
}

func (r *doctorReport) add(name, status, format string, args ...any) {
	r.Checks = append(r.Checks, doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
	if status == checkFail {
		r.OK = false
	}
}

func addDoctor() {
	var maxAge time.Duration
	command := &cobra.Command{Use: "doctor", GroupID: groupInspect, Short: "Check configuration, token, database freshness, index and encoder", Args: cobra.NoArgs, Annotations: map[string]string{configOptional: "true"}, RunE: func(c *cobra.Command, _ []string) error {
		report := runDoctor(c.Context(), maxAge)
		if err := output(c, report); err != nil {
			return err
		}
		if !report.OK {
			return errDoctorFailed
		}
		return nil
	}}
	command.Flags().DurationVar(&maxAge, "max-age", 24*time.Hour, "Fail when the last collection is older than this")
	addEmbeddingFlags(command)
	root.AddCommand(command)
}

func runDoctor(ctx context.Context, maxAge time.Duration) doctorReport {
	report := doctorReport{OK: true, Checks: []doctorCheck{}}
	if setupErr != nil {
		report.add("configuration", checkFail, "%v", setupErr)
		return report
	}
	report.add("configuration", checkOK, "%d repositories configured", len(settings.Repositories))
	if os.Getenv(settings.TokenEnv) == "" {
		report.add("github token", checkWarn, "%s is not set; sync needs it, local queries do not", settings.TokenEnv)
	} else {
		report.add("github token", checkOK, "%s is set", settings.TokenEnv)
	}
	path, err := databasePath()
	if err != nil {
		report.add("database", checkFail, "%v", err)
		return report
	}
	report.DBPath = path
	status, err := doctorStatus(ctx)
	if err != nil {
		report.add("database", checkFail, "%v", err)
	} else {
		report.add("database", checkOK, "%s opens with %d tickets and %d comments", path, status.Issues, status.Comments)
		doctorFreshness(&report, status, maxAge)
		if pending, err := findPendingSync(ctx, path); err != nil {
			report.add("pending sync", checkWarn, "%v", err)
		} else if pending != nil && pending.Pending != nil && pending.Failures > 0 {
			report.add("pending sync", checkFail, "%d failed attempts since %s; last error: %s; fix the cause or run gh-mirror sync --restart",
				pending.Failures, pending.Started.UTC().Format(time.RFC3339), pending.LastError)
		} else if pending != nil {
			report.add("pending sync", checkWarn, "unfinished sync work saved at %s; the next sync resumes it", pending.UpdatedAt)
		}
		doctorSemantic(&report, status.Semantic)
		doctorCommentPolicy(&report, status.CommentFiltering)
	}
	if state, err := checkEncoder(ctx); err != nil {
		report.add("encoder", checkFail, "%v", err)
	} else if state.FallbackReason != "" {
		report.add("encoder", checkWarn, "%s on %s after fallback: %s", state.Backend, state.Device, state.FallbackReason)
	} else {
		report.add("encoder", checkOK, "%s on %s matches the bundled model", state.Backend, state.Device)
	}
	return report
}

func doctorStatus(ctx context.Context) (store.Status, error) {
	db, err := open(true)
	if err != nil {
		return store.Status{}, err
	}
	defer closeStore(ctx, db)
	status, err := db.Status(ctx)
	if err != nil {
		return status, fmt.Errorf("read local status: %w", err)
	}
	return status, nil
}

func doctorFreshness(report *doctorReport, status store.Status, maxAge time.Duration) {
	age, collected, err := status.CollectionAge(time.Now())
	if err != nil {
		report.add("collection age", checkFail, "%v", err)
		return
	}
	if !collected {
		report.add("collection age", checkFail, "the mirror has not been collected; run gh-mirror sync")
		return
	}
	age = age.Round(time.Minute)
	if age > maxAge {
		report.add("collection age", checkFail, "collected %s ago, older than %s", age, maxAge)
		return
	}
	report.add("collection age", checkOK, "collected %s ago", age)
}

func doctorSemantic(report *doctorReport, s *store.SemanticStatus) {
	switch {
	case s == nil:
		report.add("semantic index", checkWarn, "no semantic index; search falls back to lexical; run gh-mirror index")
	case !s.Compatible:
		report.add("semantic index", checkWarn, "index model %s does not match the bundled encoder; run gh-mirror index", s.Model)
	case s.PendingDocuments > 0:
		report.add("semantic index", checkWarn, "%d/%d documents indexed, %d pending; run gh-mirror index", s.IndexedDocuments, s.Documents, s.PendingDocuments)
	default:
		report.add("semantic index", checkOK, "%d/%d documents indexed", s.IndexedDocuments, s.Documents)
	}
}

func doctorCommentPolicy(report *doctorReport, saved *store.CommentFiltering) {
	if saved == nil {
		report.add("PR comment policy", checkWarn, "no saved policy; run gh-mirror index or sync to apply the configured policy")
		return
	}
	stale := []string{}
	for repo, configured := range settings.CommentPolicies() {
		want, err := json.Marshal(configured)
		if err != nil {
			report.add("PR comment policy", checkFail, "encode configured policy: %v", err)
			return
		}
		got, err := json.Marshal(saved.Policies[repo])
		if err != nil {
			report.add("PR comment policy", checkFail, "encode saved policy: %v", err)
			return
		}
		if !bytes.Equal(want, got) {
			stale = append(stale, repo)
		}
	}
	if saved.RegistryVersion != commentpolicy.RegistryVersion {
		report.add("PR comment policy", checkWarn, "saved registry %s is older than bundled %s; run gh-mirror index", saved.RegistryVersion, commentpolicy.RegistryVersion)
		return
	}
	if len(stale) > 0 {
		slices.Sort(stale)
		report.add("PR comment policy", checkWarn, "configured policy differs from the saved one for %s; run gh-mirror index", strings.Join(stale, ", "))
		return
	}
	report.add("PR comment policy", checkOK, "registry %s, %d comments excluded", saved.RegistryVersion, saved.Excluded)
}
