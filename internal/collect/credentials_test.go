package collect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func TestCredentialScope(t *testing.T) {
	for _, name := range []string{"unchanged", "changed", "anonymous to token", "token removed", "legacy identity", "cancelled rebuild", "failed rebuild"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, c, f, started := setup(t, 1)
			initialToken := "synthetic-first"
			if name == "anonymous to token" {
				initialToken = ""
			}
			t.Setenv(c.TokenEnv, initialToken)
			var legacyVisible atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/issues" {
					f.ServeHTTP(w, r)
					return
				}
				items := []any{}
				if r.URL.Query().Get("since") == "" {
					count := 1
					if r.Header.Get("Authorization") == "Bearer synthetic-second" || legacyVisible.Load() || (name == "token removed" && r.Header.Get("Authorization") != "") {
						count = 2
					}
					for n := 1; n <= count; n++ {
						items = append(items, f.issue(n))
					}
				}
				if err := json.NewEncoder(w).Encode(items); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c.APIURL, c.GraphQLURL, f.url = server.URL, server.URL+"/graphql", server.URL
			initial, err := syncAt(ctx, db, c, Options{}, started)
			if err != nil {
				t.Fatal(err)
			}
			token := "synthetic-second"
			if name == "unchanged" || name == "legacy identity" {
				token = initialToken
			}
			if name == "token removed" {
				token = ""
			}
			t.Setenv(c.TokenEnv, token)
			if name == "legacy identity" {
				legacyVisible.Store(true)
				if err := db.Update(ctx, func(w *store.Writer) error { return w.SetMetadata(ctx, "credential_identity", "") }); err != nil {
					t.Fatal(err)
				}
			}
			changedAt := started.Add(10 * time.Minute)
			var phases []string
			options := Options{Progress: func(e progress.Event) { phases = append(phases, e.Phase) }}
			interrupted := name == "cancelled rebuild" || name == "failed rebuild"
			if interrupted {
				stopCtx, cancel := context.WithCancel(ctx)
				c.Workers = 1
				f.failGraph = name == "failed rebuild"
				if name == "cancelled rebuild" {
					options.Progress = func(e progress.Event) {
						if e.Resource == progress.FetchingIssues && e.Page == 1 {
							cancel()
						}
					}
				}
				if _, err := syncAt(stopCtx, db, c, options, changedAt); err == nil || (name == "cancelled rebuild" && !errors.Is(err, context.Canceled)) {
					t.Fatal("rebuild did not fail", err)
				}
				cancel()
				after, err := db.Status(ctx)
				if err != nil || after.Generation != initial.Status.Generation || after.Issues != initial.Status.Issues {
					t.Fatal("partial scope became visible", after, err)
				}
				if err := db.Update(ctx, func(w *store.Writer) error {
					identity, err := w.Metadata(ctx, "credential_identity")
					if err == nil && identity != credentialIdentity(c, initialToken) {
						t.Fatal("failed rebuild advanced credential identity")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.failGraph = false
				options.Progress = func(e progress.Event) { phases = append(phases, e.Phase) }
			}
			invokedAt := changedAt
			if interrupted {
				invokedAt = changedAt.Add(time.Minute)
			}
			result, err := syncAt(ctx, db, c, options, invokedAt)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if name == "unchanged" || name == "token removed" {
				want = 1
			}
			if result.Status.Issues != want {
				t.Fatal("scope inventory missed newly visible old issues", result)
			}
			if strings.Contains(strings.Join(phases, "\n"), "Credential scope") != (name != "unchanged") {
				t.Fatal("missing credential scope progress", phases)
			}
			if name == "unchanged" && result.Requests != 2 {
				t.Fatal("unchanged credentials forced full sync", result)
			}
			if interrupted && (result.Resumed == 0 || result.Status.CollectedAt != changedAt.Format(time.RFC3339Nano)) {
				t.Fatal("credential rebuild lost saved work", result)
			}
			if err := db.Update(ctx, func(w *store.Writer) error {
				identity, err := w.Metadata(ctx, "credential_identity")
				if err == nil && (identity != credentialIdentity(c, token) || identity == token) {
					t.Fatal("credential identity missing or stored token")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			next, err := syncAt(ctx, db, c, Options{}, changedAt.Add(2*time.Minute))
			if err != nil || next.Requests != 2 {
				t.Fatal("rebuilt mirror did not return to incremental sync", next, err)
			}
		})
	}
}
