package representation

import (
	"slices"
	"strings"
	"testing"
)

func TestAttributes(t *testing.T) {
	for _, name := range []string{"multiple clients", "exact override", "longest prefix", "invalid rules"} {
		t.Run(name, func(t *testing.T) {
			rules := []LabelRule{{Prefix: "customer:", Role: "client"}, {Prefix: "customer:vip:", Role: "partner"}, {Label: "customer:vip:Acme", Role: "client", Value: "Acme", Aliases: []string{"Example"}}}
			a := Attributes([]string{"client:A", "client:B", "customer:vip:Acme", "customer:vip:Other", "bug"}, rules)
			switch name {
			case "multiple clients":
				if a[0].Role != "client" || a[1].Role != "client" || len(a) != 5 {
					t.Fatal(a)
				}
			case "exact override":
				if a[2].Value != "Acme" || a[2].Role != "client" || a[2].Aliases[0] != "Example" {
					t.Fatal(a)
				}
			case "longest prefix":
				if a[3].Role != "partner" || a[3].Value != "Other" || a[4].Role != "label" {
					t.Fatal(a)
				}
			case "invalid rules":
				if ValidateRules(map[string][]LabelRule{"o/r": {{Label: "bug", Prefix: "type:", Role: "type"}}}) == nil {
					t.Fatal("ambiguous rule accepted")
				}
			}
		})
	}
}

func TestSectionsAndRetrieval(t *testing.T) {
	for _, name := range []string{"unicode offsets", "code preserved", "empty sections", "template excluded"} {
		t.Run(name, func(t *testing.T) {
			body := "### Description\r\nRésumé fails\r\n```go\n### expected inside code\n```\n### Expected behavior\n_No response_\n### Steps to reproduce\n1. Click again\n### Before requesting the review, make sure you've:\n- [x] ordinary template\n"
			sections := Sections(body)
			if len(sections) != 4 {
				t.Fatal(sections)
			}
			switch name {
			case "unicode offsets":
				if !strings.HasPrefix(body[sections[0].Start:sections[0].End], "Résumé") {
					t.Fatal(sections)
				}
			case "code preserved":
				if !strings.Contains(body[sections[0].Start:sections[0].End], "### expected inside code") {
					t.Fatal(sections)
				}
			case "empty sections":
				if !sections[1].Empty || sections[1].Kind != "expected" {
					t.Fatal(sections)
				}
			case "template excluded":
				spans := RetrievalSpans(body)
				if len(spans) != 2 || strings.Contains(body[spans[1].Start:spans[1].End], "template") {
					t.Fatal(spans)
				}
			}
		})
	}
}

func TestReferences(t *testing.T) {
	for _, name := range []string{"typed links", "code ignored", "foreign URLs"} {
		t.Run(name, func(t *testing.T) {
			body := "Duplicate of #2; fixes o/s#4; see https://github.com/o/r/issues/6\n```\n#99\n```\nhttps://example.com/o/r/issues/7"
			r := References("o/r", body, "https://github.com/o/r/issues/1")
			if len(r) != 3 {
				t.Fatal(r)
			}
			if name == "typed links" && (r[0].Kind != "duplicate_reference" || r[1].Kind != "fix_reference" || r[2].Kind != "mention" || body[r[0].Start:r[0].End] != "#2") {
				t.Fatal(r)
			}
		})
	}
}

func TestLongFences(t *testing.T) {
	for _, name := range []string{"backticks", "tildes"} {
		t.Run(name, func(t *testing.T) {
			mark := "`"
			if name == "tildes" {
				mark = "~"
			}
			body := "### Description\n" + strings.Repeat(mark, 4) + "\n" + strings.Repeat(mark, 3) + "\n### Expected behavior\nduplicate of #2\n" + strings.Repeat(mark, 4) + "\n### Actual behavior\nSee #3\n"
			sections := Sections(body)
			if len(sections) != 2 || sections[1].Kind != "actual" {
				t.Fatal(sections)
			}
			refs := References("o/r", body, "https://github.com/o/r/issues/1")
			if len(refs) != 1 || refs[0].Number != 3 {
				t.Fatal(refs)
			}
		})
	}
}

func TestInlineTripleBackticks(t *testing.T) {
	body := "### Description\n```npm ci``` fails, see #7\n### Expected behavior\nInstall works\n"
	t.Run("sections", func(t *testing.T) {
		sections := Sections(body)
		if len(sections) != 2 || sections[1].Kind != "expected" {
			t.Fatal(sections)
		}
	})
	t.Run("references", func(t *testing.T) {
		refs := References("o/r", body, "https://github.com/o/r/issues/1")
		if len(refs) != 1 || refs[0].Number != 7 {
			t.Fatal(refs)
		}
	})
}

func TestReferenceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []Reference
	}{
		{"documentation fragment", "See https://docs.example.com/guide#123 for details", nil},
		{"version string", "Version 1.2/3.4#5 fails", nil},
		{"word before shorthand", "build abc#5 and x/o/r#6", nil},
		{"invalid owner", "-o/r#5 and o_x/r#6", nil},
		{"comment fragment", "https://github.com/o/r/issues/5#issuecomment-99", []Reference{{Repo: "o/r", Number: 5, Start: 0, End: 31}}},
		{"pull request subpage", "see https://github.com/o/s/pull/8/files.", []Reference{{Repo: "o/s", Number: 8, Start: 4, End: 33}}},
		{"trailing punctuation", "(o/s#9).", []Reference{{Repo: "o/s", Number: 9, Start: 1, End: 6}}},
		{"foreign issue URL", "https://gitlab.com/o/r/issues/5", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := []Reference{}
			for _, r := range References("o/r", tc.body, "https://github.com/o/r/issues/1") {
				got = append(got, Reference{Repo: r.Repo, Number: r.Number, Start: r.Start, End: r.End})
			}
			if tc.want == nil {
				tc.want = []Reference{}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
		})
	}
}
