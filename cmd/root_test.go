package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs([]string{"--version"})
		if err := Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(stdout.String()) != Version || stderr.Len() != 0 {
			t.Fatal(stdout.String(), stderr.String())
		}
		if root.Flags().Lookup("version").Shorthand != "" {
			t.Fatal("version gained a short alias")
		}
		for _, command := range root.Commands() {
			for _, name := range []string{"debug", "trace", "config", "help"} {
				flag := command.InheritedFlags().Lookup(name)
				if flag == nil {
					t.Fatalf("%s did not inherit %s", command.Name(), name)
				}
				if name != "debug" && name != "config" && flag.Shorthand != "" {
					t.Fatalf("unexpected alias on %s", name)
				}
			}
		}
	})
}
