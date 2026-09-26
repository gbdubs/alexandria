package archive

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepositoryFromWorktree(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "pharos")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	git("commit", "--allow-empty", "-qm", "initial")
	worktree := filepath.Join(t.TempDir(), "halifax")
	git("worktree", "add", "-qb", "halifax", worktree)
	got := repositoryFromLocation(worktree, "")
	if got == nil || got["display_name"] != "pharos" {
		t.Fatalf("repository = %#v", got)
	}
	git("remote", "add", "origin", "git@github.com:example/pharos.git")
	got = repositoryFromLocation(worktree, "")
	if got["display_name"] != "pharos" || got["canonical_remote"] != "git@github.com:example/pharos.git" {
		t.Fatalf("repository with remote = %#v", got)
	}
}

func TestRepositoryFromMissingConductorWorkspace(t *testing.T) {
	got := repositoryFromLocation("/old/conductor/workspaces/pharos/halifax", "")
	if got == nil || got["display_name"] != "pharos" {
		t.Fatalf("repository = %#v", got)
	}
	if got := repositoryFromLocation("/old/unknown/halifax", ""); got != nil {
		t.Fatalf("unverified repository = %#v", got)
	}
}
