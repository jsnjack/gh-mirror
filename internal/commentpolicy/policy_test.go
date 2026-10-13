package commentpolicy

import (
	"testing"
)

func TestKnownAI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		author Author
		want   string
	}{
		{"Copilot without suffix", Author{Login: "Copilot", Type: "Bot"}, "copilot"},
		{"Copilot alias", Author{Login: "copilot-pull-request-reviewer[bot]", Type: "Bot"}, "copilot"},
		{"renamed account", Author{Login: "reviewer", Type: "Bot", ID: "175728472", Host: "github.com"}, "copilot"},
		{"enterprise IDs", Author{Login: "reviewer", Type: "Bot", ID: "175728472", Host: "github.example.com"}, ""},
		{"human name", Author{Login: "Copilot", Type: "User"}, ""},
		{"missing type", Author{Login: "Copilot"}, ""},
		{"CodeRabbit", Author{Login: "coderabbitai[bot]", Type: "Bot"}, "coderabbit"},
		{"Gemini", Author{Login: "gemini-code-assist[bot]", Type: "Bot"}, "gemini"},
		{"dependency bot", Author{Login: "dependabot[bot]", Type: "Bot"}, ""},
		{"security bot", Author{Login: "github-advanced-security[bot]", Type: "Bot"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KnownAI(tc.author); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	for _, name := range []string{"AI threads", "AI comments only", "explicit allow", "custom author", "all bots", "missing parent", "cross ticket parent", "cycle"} {
		t.Run(name, func(t *testing.T) {
			p := Policy{ExcludeKnownAI: true, ExcludeAIThreads: true}
			comments := []Comment{
				{ID: "review:1", Repo: "o/r", Number: 1, Author: Author{Login: "Copilot", Type: "Bot"}},
				{ID: "review:2", Repo: "o/r", Number: 1, Parent: "review:1", Author: Author{Login: "alice", Type: "User"}},
				{ID: "review:3", Repo: "o/r", Number: 1, Parent: "review:2", Author: Author{Login: "bob", Type: "User"}},
				{ID: "review:4", Repo: "o/r", Number: 1, Author: Author{Login: "alice", Type: "User"}},
				{ID: "review:5", Repo: "o/r", Number: 1, Parent: "review:4", Author: Author{Login: "Copilot", Type: "Bot"}},
				{ID: "6", Repo: "o/r", Number: 1, Author: Author{Login: "dependabot[bot]", Type: "Bot"}},
				{ID: "7", Repo: "o/r", Number: 1, Author: Author{Login: "custom-reviewer", Type: "User"}},
			}
			switch name {
			case "AI comments only":
				p.ExcludeAIThreads = false
			case "explicit allow":
				p.IncludeAuthors = []string{"ALICE"}
			case "custom author":
				p.ExcludeAuthors = []string{"custom-reviewer"}
			case "all bots":
				p.ExcludeAllBots = true
			case "missing parent":
				comments[1].Parent = "review:99"
			case "cross ticket parent":
				comments[1].Number = 2
			case "cycle":
				comments[0].Parent = "review:3"
			}
			out, err := Select(comments, map[string]Policy{"o/r": p})
			if name == "cycle" {
				if err == nil {
					t.Fatal("reply cycle accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out["review:1"].Reason == "" || out["review:5"].Reason == "" || out["review:4"].Reason != "" {
				t.Fatal(out)
			}
			wantHuman := name != "AI comments only" && name != "explicit allow" && name != "missing parent" && name != "cross ticket parent"
			if (out["review:2"].Reason != "") != wantHuman {
				t.Fatal(out)
			}
			if name == "AI threads" && (out["review:3"].Root != "review:1" || out["6"].Reason != "") {
				t.Fatal(out)
			}
			if name == "custom author" && out["7"].Reason != "excluded_author" {
				t.Fatal(out)
			}
			if name == "all bots" && out["6"].Reason != "bot" {
				t.Fatal(out)
			}
		})
	}
}

func TestPolicyOverrides(t *testing.T) {
	for _, name := range []string{"inherit", "false override", "replace list", "empty author", "wildcard", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			p := Policy{ExcludeKnownAI: true, ExcludeAuthors: []string{"custom"}}
			var o Override
			switch name {
			case "false override":
				v := false
				o.ExcludeKnownAI = &v
			case "replace list":
				v := []string{}
				o.ExcludeAuthors = &v
			case "empty author":
				p.ExcludeAuthors = []string{""}
			case "wildcard":
				p.ExcludeAuthors = []string{"*ai*"}
			case "duplicate":
				p.ExcludeAuthors = []string{"Custom", "custom"}
			}
			p = p.Resolve(o)
			if (p.Validate() != nil) != (name == "empty author" || name == "wildcard" || name == "duplicate") {
				t.Fatal(p)
			}
			if name == "false override" && p.ExcludeKnownAI {
				t.Fatal(p)
			}
			if name == "replace list" && len(p.ExcludeAuthors) != 0 {
				t.Fatal(p)
			}
		})
	}
}
