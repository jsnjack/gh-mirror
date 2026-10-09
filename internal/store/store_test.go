package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCollectionVersion(t *testing.T) {
	for _, name := range []string{"legacy", "current", "other contract", "malformed"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			version := LegacyCollectionVersion
			if name != "legacy" {
				version = CollectionVersion
				if name == "other contract" {
					version++
				}
				value := strconv.Itoa(version)
				if name == "malformed" {
					value = "invalid"
				}
				if err := db.Update(ctx, func(w *Writer) error { return w.SetMetadata(ctx, "collection_version", value) }); err != nil {
					t.Fatal(err)
				}
			}
			status, err := db.Status(ctx)
			if name == "malformed" {
				if err == nil || !strings.Contains(err.Error(), "parse collection version") {
					t.Fatal("malformed version was silently accepted", status, err)
				}
			} else if err != nil || status.CollectionVersion != version {
				t.Fatal("incorrect collection compatibility metadata", status, err)
			}
		})
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "mirror.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}
func issueFixture(number int, body, updated string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"I_%d","title":"Connection crash","body":%q,"state":"closed","updated_at":%q,"html_url":"https://github.com/o/r/issues/%d","labels":[{"name":"bug"}],"type":{"name":"Bug"},"assignees":[{"login":"a"},{"login":"b"}]}`, number, number, body, updated, number))
}
func commentFixture(body, updated string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":9007199254740993,"body":%q,"updated_at":%q,"html_url":"https://github.com/o/r/issues/1#issuecomment-1","created_at":"2026-01-01T00:00:00Z"}`, body, updated))
}
func seed(t *testing.T, db *Store) {
	t.Helper()
	err := db.Update(context.Background(), func(w *Writer) error {
		for _, n := range []int{1, 2} {
			if _, err := w.PutIssue(context.Background(), "o/r", issueFixture(n, "network failure", "2026-01-02T00:00:00Z")); err != nil {
				return fmt.Errorf("seed issue: %w", err)
			}
		}
		if err := w.PutComment(context.Background(), "o/r", 1, commentFixture("uniqueword socket hang", "2026-01-02T00:00:00Z")); err != nil {
			return fmt.Errorf("seed comment: %w", err)
		}
		for k, v := range map[string]string{"generation": "testgeneration", "repositories": `["o/r"]`, "collected_at": "2026-01-02T00:00:00Z"} {
			if err := w.SetMetadata(context.Background(), k, v); err != nil {
				return fmt.Errorf("seed metadata: %w", err)
			}
		}
		return w.SetCoverage(context.Background(), Coverage{Repo: "o/r", CollectedAt: "2026-01-02T00:00:00Z", ReconciledAt: "2026-01-02T00:00:00Z", Fields: "complete", Projects: "complete"})
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestMirror(t *testing.T) {
	for _, name := range []string{"comments search", "literal operators", "filters", "get complete", "candidates exclude seed", "rollback", "stale updates", "edited deleted comments", "reader missing", "collector exclusion", "catalog cache scope", "export", "schema rejection", "transferred issue comments"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			switch name {
			case "transferred issue comments":
				err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, "o/s", issueFixture(1, "network failure", "2026-01-03T00:00:00Z"))
					if err != nil {
						return fmt.Errorf("transfer issue: %w", err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := db.Get(ctx, "o/s", 1)
				if err != nil || len(out.Comments) != 1 {
					t.Fatal("transfer lost historical comments", out, err)
				}
				matches, err := db.Search(ctx, SearchOptions{Query: "uniqueword"})
				if err != nil || len(matches.Matches) != 1 || matches.Matches[0].Repo != "o/s" {
					t.Fatal("transfer index retained old repository", matches, err)
				}
			case "comments search", "literal operators", "filters":
				options := SearchOptions{Query: "uniqueword"}
				if name == "literal operators" {
					options.Query = `uniqueword OR title:missing " *`
				}
				if name == "filters" {
					options.Repo = "o/r"
					options.State = "closed"
					options.Label = "bug"
					options.Type = "Bug"
				}
				out, err := db.Search(ctx, options)
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Matches) != 1 || out.Matches[0].Number != 1 || !strings.Contains(out.Matches[0].Source, "issuecomment") {
					t.Fatalf("unexpected matches: %+v", out)
				}
				if out.Status.Generation != "testgeneration" {
					t.Fatal("missing generation")
				}
			case "get complete":
				out, err := db.Get(ctx, "o/r", 1)
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Comments) != 1 || !strings.Contains(string(out.Payload), `"login":"b"`) || !strings.Contains(string(out.Comments[0]), "9007199254740993") {
					t.Fatalf("lost payloads: %+v", out)
				}
				if _, err := db.Get(ctx, "o/r", 999); !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
			case "candidates exclude seed":
				out, err := db.Candidates(ctx, "o/r", 1, 20)
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Matches) != 1 || out.Matches[0].Number != 2 || out.Matches[0].State != "closed" {
					t.Fatalf("unexpected candidates: %+v", out)
				}
			case "rollback":
				err := db.Update(ctx, func(w *Writer) error {
					if _, err := w.PutIssue(ctx, "o/r", issueFixture(3, "rollbackword", "2026-01-03T00:00:00Z")); err != nil {
						return fmt.Errorf("put issue: %w", err)
					}
					return fmt.Errorf("upstream failed")
				})
				if err == nil {
					t.Fatal("expected failure")
				}
				out, err := db.Search(ctx, SearchOptions{Query: "rollbackword"})
				if err != nil || len(out.Matches) != 0 {
					t.Fatalf("rollback leaked: %+v %v", out, err)
				}
			case "stale updates":
				err := db.Update(ctx, func(w *Writer) error {
					ref, err := w.PutIssue(ctx, "o/r", issueFixture(1, "older", "2026-01-01T00:00:00Z"))
					if err != nil {
						return fmt.Errorf("put stale issue: %w", err)
					}
					if ref.Changed {
						t.Fatal("stale response marked changed")
					}
					return w.PutComment(ctx, "o/r", 1, commentFixture("older", "2026-01-01T00:00:00Z"))
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := db.Search(ctx, SearchOptions{Query: "uniqueword"})
				if err != nil || len(out.Matches) != 1 {
					t.Fatalf("stale comment overwrote: %v %v", out, err)
				}
			case "edited deleted comments":
				err := db.Update(ctx, func(w *Writer) error {
					return w.PutComment(ctx, "o/r", 1, commentFixture("replacementword", "2026-01-03T00:00:00Z"))
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := db.Search(ctx, SearchOptions{Query: "uniqueword"})
				if err != nil || len(out.Matches) != 0 {
					t.Fatal("old comment index remained", err)
				}
				if err := db.Update(ctx, func(w *Writer) error { return w.Reconcile(ctx, "o/r", map[int]bool{1: true}, map[string]bool{}) }); err != nil {
					t.Fatal(err)
				}
				out, err = db.Search(ctx, SearchOptions{Query: "replacementword network"})
				if err != nil || len(out.Matches) != 1 || out.Matches[0].Number != 1 {
					t.Fatalf("deleted records remained: %+v %v", out, err)
				}
			case "reader missing":
				if _, err := Open(filepath.Join(t.TempDir(), "absent.sqlite"), true); err == nil {
					t.Fatal("reader created missing database")
				}
			case "collector exclusion":
				collector, err := Open(db.Path, false)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := collector.Close(); err != nil {
						t.Error(err)
					}
				}()
				collector.db.SetMaxOpenConns(1)
				if _, err := collector.db.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
					t.Fatal(err)
				}
				entered := make(chan struct{})
				release := make(chan struct{})
				done := make(chan error, 1)
				go func() { done <- db.Update(ctx, func(*Writer) error { close(entered); <-release; return nil }) }()
				<-entered
				other, err := Open(db.Path, true)
				if err != nil {
					close(release)
					t.Fatal(err)
				}
				if _, err := other.Status(ctx); err != nil {
					t.Error("reader blocked by collector", err)
				}
				if err := other.Close(); err != nil {
					t.Error(err)
				}
				if err := collector.Update(ctx, func(*Writer) error { return nil }); err == nil {
					t.Error("concurrent collector obtained lock")
				}
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			case "catalog cache scope":
				err := db.Update(ctx, func(w *Writer) error {
					if err := w.ReplaceCatalog(ctx, "labels", "o/r", []json.RawMessage{json.RawMessage(`{"id":1,"name":"unused"}`)}); err != nil {
						return fmt.Errorf("put catalog: %w", err)
					}
					if err := w.Cache(ctx, "url", "etag", []byte(`{"body":[]}`)); err != nil {
						return fmt.Errorf("put cache: %w", err)
					}
					etag, body, err := w.Cached(ctx, "url")
					if err != nil || etag != "etag" || len(body) == 0 {
						t.Fatalf("cache: %q %s %v", etag, body, err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := db.Catalog(ctx, "labels", "o/r")
				if err != nil || len(out.Items) != 1 {
					t.Fatal(out, err)
				}
				if err := db.Update(ctx, func(w *Writer) error { return w.ResetScope(ctx, []string{"other/repo"}, true, false) }); err != nil {
					t.Fatal(err)
				}
				status, err := db.Status(ctx)
				if err != nil || status.Issues != 0 || status.Comments != 0 {
					t.Fatal(status, err)
				}
			case "export":
				dest := filepath.Join(t.TempDir(), "snapshot.sqlite")
				if err := db.Export(ctx, dest); err != nil {
					t.Fatal(err)
				}
				reader, err := Open(dest, true)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				}()
				if err := reader.Validate(ctx); err != nil {
					t.Fatal(err)
				}
				out, err := reader.Search(ctx, SearchOptions{Query: "uniqueword"})
				if err != nil || len(out.Matches) != 1 {
					t.Fatal(out, err)
				}
			case "schema rejection":
				if err := db.Update(ctx, func(w *Writer) error { return w.SetMetadata(ctx, "schema_version", "999") }); err != nil {
					t.Fatal(err)
				}
				if unexpected, err := Open(db.Path, false); err == nil {
					if err := unexpected.Close(); err != nil {
						t.Error(err)
					}
					t.Fatal("writer accepted future schema")
				}
			}
		})
	}
}
func TestJSONIdentities(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{{"integer", `{"id":9007199254740993}`, "9007199254740993"}, {"string", `{"id":"node"}`, "node"}, {"invalid", `null`, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			object, err := Object(json.RawMessage(tc.raw))
			if tc.name == "invalid" {
				if err == nil {
					t.Fatal("accepted null")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := Identity(object, "id"); got != tc.want {
				t.Fatal(got)
			}
		})
	}
}
