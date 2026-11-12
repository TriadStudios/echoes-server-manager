package testkit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every action a workflow uses is pinned to a full commit SHA (a tag can
// be moved to other code), with the tag it stands for in a comment.
func TestWorkflowActionsArePinned(t *testing.T) {
	root := moduleRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found")
	}
	uses := regexp.MustCompile(`^\s*-?\s*uses:\s*(\S+)(.*)$`)
	pinned := regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			m := uses.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinned.MatchString(m[1]) || !strings.Contains(m[2], "# v") {
				t.Errorf("%s:%d: %s is not pinned to a commit SHA with its tag in a comment", filepath.Base(f), i+1, m[1])
			}
		}
	}
}
