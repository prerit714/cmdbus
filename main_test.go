package main

import (
	"os"
	"strings"
	"testing"
)

func TestDocsAreTheWholeReadme(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if docs != string(readme) {
		t.Fatal("embedded docs differ from README.md")
	}
	for _, section := range []string{"## Install", "## Protocol", "## Guarantees", "## Logs", "cmdbus docs"} {
		if !strings.Contains(docs, section) {
			t.Errorf("docs lack %q", section)
		}
	}
}
