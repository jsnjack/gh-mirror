package markdown

import (
	"slices"
	"strings"
	"testing"
)

func TestFence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		delimiters []int
		open       bool
	}{
		{"backticks", "a\n```go\ncode\n```\nb", []int{1, 3}, false},
		{"tildes", "~~~\n```\n~~~", []int{0, 2}, false},
		{"longer closing", "```\nx\n`````", []int{0, 2}, false},
		{"shorter fence stays code", "````\n```\n````", []int{0, 2}, false},
		{"info string does not close", "```\n``` go\n```", []int{0, 2}, false},
		{"other delimiter stays code", "```\n~~~\n```", []int{0, 2}, false},
		{"inline triple backticks", "```npm ci``` fails\n## Heading", nil, false},
		{"tilde info may contain backticks", "~~~ `x`\ncode", []int{0}, true},
		{"indented list fence", "- step\n  ```\n  code\n  ```", []int{1, 3}, false},
		{"two backticks", "``x``\ntext", nil, false},
		{"unclosed", "```\ncode", []int{0}, true},
		{"carriage returns", "```\r\ncode\r\n```\r\n", []int{0, 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f Fence
			var got []int
			for n, line := range strings.Split(tc.body, "\n") {
				if f.Line(line) {
					got = append(got, n)
				}
			}
			if !slices.Equal(got, tc.delimiters) || f.Open() != tc.open {
				t.Fatalf("delimiters %v open %v; want %v open %v", got, f.Open(), tc.delimiters, tc.open)
			}
		})
	}
}
