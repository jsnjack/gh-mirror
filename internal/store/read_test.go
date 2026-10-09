package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func seedReview(t *testing.T, db *Store) {
	t.Helper()
	if err := db.Update(context.Background(), func(w *Writer) error {
		return w.PutComment(context.Background(), "o/r", 1, json.RawMessage(`{"id":9007199254740993,"body":"review evidence","html_url":"https://github.com/o/r/pull/1#discussion_r1","pull_request_url":"https://api.github.com/repos/o/r/pulls/1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","path":"main.go","original_line":42,"diff_hunk":"@@ context @@"}`))
	}); err != nil {
		t.Fatal(err)
	}
}
func TestBoundedReads(t *testing.T) {
	for _, name := range []string{"summary", "none", "full", "page", "discussion", "review", "descending", "preview", "batch", "catalog", "bad options", "generation"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			seedReview(t, db)
			switch name {
			case "summary", "none", "full", "page":
				o := ReadOptions{Repo: "o/r", Number: 1}
				switch name {
				case "summary":
					o.View = "summary"
				case "none":
					o.Comments = "none"
				case "page":
					o.Comments = "page"
					o.Limit = 1
				}
				out, err := db.Read(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				if out.Summary == nil {
					t.Fatal("missing summary")
				}
				if name == "summary" && (out.Payload != nil || len(out.Comments) != 0) {
					t.Fatal("unbounded summary", out)
				}
				if name == "none" && len(out.Comments) != 0 {
					t.Fatal(out)
				}
				if name == "full" && len(out.Comments) != 2 {
					t.Fatal(out)
				}
				if name == "page" && (out.CommentPage == nil || len(out.CommentPage.Comments) != 1 || !out.CommentPage.HasMore) {
					t.Fatal(out)
				}
			case "discussion", "review", "descending", "generation":
				o := CommentOptions{Repo: "o/r", Number: 1, Limit: 1}
				if name == "discussion" || name == "review" {
					o.Kind = name
				}
				if name == "descending" {
					o.Order = "desc"
				}
				first, err := db.Comments(ctx, o)
				if err != nil || len(first.Comments) != 1 {
					t.Fatal(first, err)
				}
				if name == "review" && (first.Comments[0].Path != "main.go" || first.Comments[0].Line != 42) {
					t.Fatal(first)
				}
				if name == "discussion" || name == "review" {
					if first.HasMore || first.Comments[0].Kind != name {
						t.Fatal(first)
					}
					return
				}
				if !first.HasMore {
					t.Fatal(first)
				}
				o.Cursor = first.NextCursor
				if name == "generation" {
					if err := db.Update(ctx, func(w *Writer) error { return w.SetMetadata(ctx, "generation", "new") }); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Comments(ctx, o); !errors.Is(err, ErrCursorConflict) {
						t.Fatal(err)
					}
					return
				}
				last, err := db.Comments(ctx, o)
				if err != nil || len(last.Comments) != 1 || last.HasMore || last.Comments[0].Kind == first.Comments[0].Kind {
					t.Fatal(last, err)
				}
			case "preview":
				if err := db.Update(ctx, func(w *Writer) error {
					return w.PutComment(ctx, "o/r", 1, commentFixture(strings.Repeat("é", 1100), "2026-01-03T00:00:00Z"))
				}); err != nil {
					t.Fatal(err)
				}
				out, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: 1, Kind: "discussion"})
				if err != nil || !out.Comments[0].Truncated || len([]rune(out.Comments[0].Body)) != 1024 || out.Comments[0].Payload != nil {
					t.Fatal(out, err)
				}
				full, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: 1, Kind: "discussion", View: "full"})
				if err != nil || full.Comments[0].Payload == nil || len([]rune(full.Comments[0].Body)) != 1100 {
					t.Fatal(full, err)
				}
			case "batch":
				out, err := db.Batch(ctx, BatchOptions{Tickets: []TicketID{{"o/r", 2}, {"o/r", 1}, {"o/r", 99}, {"o/r", 2}}})
				if err != nil || len(out.Issues) != 2 || len(out.Missing) != 1 || out.Issues[0].Number != 2 || out.Issues[0].Payload != nil {
					t.Fatal(out, err)
				}
			case "catalog":
				if err := db.Update(ctx, func(w *Writer) error {
					return w.ReplaceCatalog(ctx, "labels", "o/r", []json.RawMessage{json.RawMessage(`{"id":1,"name":"one"}`), json.RawMessage(`{"id":2,"name":"two"}`)})
				}); err != nil {
					t.Fatal(err)
				}
				o := CatalogOptions{Kind: "labels", Scope: "o/r", Limit: 1}
				first, err := db.CatalogPage(ctx, o)
				if err != nil || !first.HasMore || len(first.Items) != 1 {
					t.Fatal(first, err)
				}
				o.Cursor = first.NextCursor
				last, err := db.CatalogPage(ctx, o)
				if err != nil || last.HasMore || len(last.Items) != 1 || string(last.Items[0]) == string(first.Items[0]) {
					t.Fatal(last, err)
				}
			case "bad options":
				for _, o := range []ReadOptions{{Repo: "o/r", Number: 1, Comments: "magic"}, {Repo: "o/r", Number: 1, View: "wrong"}, {Repo: "o/r", Number: 1, Limit: 1}, {Repo: "oops", Number: 1}} {
					if _, err := db.Read(ctx, o); !errors.Is(err, ErrInvalidQuery) {
						t.Fatal(o, err)
					}
				}
			}
		})
	}
}
func TestReviewEvidence(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	seedReview(t, db)
	out, err := db.Search(ctx, SearchOptions{Query: "review", In: []string{"reviews"}})
	if err != nil || len(out.Matches) != 1 {
		t.Fatal(out, err)
	}
	e := out.Matches[0].Evidence[0]
	if e.Field != "review_comment" || e.CommentID != "9007199254740993" || e.Path != "main.go" || e.Line != 42 || !strings.Contains(e.Source, "discussion_r") {
		t.Fatal(e)
	}
}
func TestQueryCoverage(t *testing.T) {
	for _, name := range []string{"disabled", "stale", "fresh"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			if err := db.Update(ctx, func(w *Writer) error {
				return w.SetMetadata(ctx, "repository_options", fmt.Sprintf(`{"o/r":{"issues":true,"projects":%t,"fields":true}}`, name != "disabled"))
			}); err != nil {
				t.Fatal(err)
			}
			age := "1h"
			if name == "fresh" {
				age = "100000h"
			}
			out, err := db.List(ctx, ListOptions{Project: "o/1", PageOptions: PageOptions{MaxEnrichmentAge: age}})
			if err != nil {
				t.Fatal(err)
			}
			if name == "fresh" {
				if len(out.Warnings) != 0 {
					t.Fatal(out.Warnings)
				}
			} else {
				if len(out.Warnings) != 1 {
					t.Fatal(out.Warnings)
				}
				want := "resource_disabled"
				if name == "stale" {
					want = "enrichment_stale"
				}
				if out.Warnings[0].Code != want {
					t.Fatal(out.Warnings)
				}
			}
		})
	}
}
