package archive

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainIntegrationFindsMergeAndDirectHistory(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Archive Test")
	git("config", "user.email", "archive@example.com")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "Base")
	git("branch", "feature")
	git("checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "Feature work")
	feature := git("rev-parse", "HEAD")
	git("checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "Main work")
	git("merge", "--no-ff", "feature", "-m", "Merge feature into main")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	merged := mainIntegration(repo, feature, "git@github.com:acme/archive.git")
	if merged == nil || merged["main_merge_method"] != "merge" || merged["main_merge_title"] != "Merge feature into main" || !strings.HasSuffix(firstString(merged["main_merge_url"]), firstString(merged["main_merge_commit"])) {
		t.Fatalf("merge integration: %#v", merged)
	}
	git("checkout", "-b", "unmerged")
	if err := os.WriteFile(filepath.Join(repo, "unmerged.txt"), []byte("unmerged"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "Unmerged work")
	unmerged := git("rev-parse", "HEAD")
	if got := mainIntegration(repo, unmerged, ""); got != nil {
		t.Fatalf("unmerged commit attributed to main: %#v", got)
	}
	git("checkout", "main")
	git("merge", "--ff-only", "unmerged")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	direct := mainIntegration(repo, unmerged, "")
	if direct == nil || direct["main_merge_method"] != "direct" || direct["main_merge_commit"] != unmerged {
		t.Fatalf("direct integration: %#v", direct)
	}
	catalog, _ := testCatalog(t)
	if _, err := catalog.DB.Exec(`INSERT INTO repositories(id,canonical_remote,display_name,local_locations_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "repo", "git@github.com:acme/archive.git", "archive", jsonText([]string{repo}), now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,repository_id,title,head_ref,indexed_at) VALUES(?,?,?,?,?,?,?,?)`, "work", "conductor", "local", "source", "repo", "Feature work", feature, now()); err != nil {
		t.Fatal(err)
	}
	// Only paths recorded on this host are consulted; another Mac's sighting of
	// the same path must not run Git here.
	if _, err := catalog.DB.Exec(`INSERT INTO workspace_sightings(workspace_id,host_id,source_name,repository_locations_json,first_seen_at,last_seen_at)
		VALUES('work','host_elsewhere','conductor',?,?,?)`, jsonText([]string{repo}), now(), now()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.refreshMainIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	var unattributed sql.NullString
	if err := catalog.DB.QueryRow(`SELECT main_merge_commit FROM workspaces WHERE id='work'`).Scan(&unattributed); err != nil || unattributed.Valid {
		t.Fatalf("another host's path was used: %v %v", unattributed, err)
	}
	if _, err := catalog.DB.Exec(`INSERT INTO workspace_sightings(workspace_id,host_id,source_name,location,first_seen_at,last_seen_at)
		VALUES('work',?,'conductor',?,?,?)`, currentHost().ID, repo, now(), now()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.refreshMainIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	var storedCommit, storedTitle string
	if err := catalog.DB.QueryRow(`SELECT main_merge_commit,main_merge_title FROM workspaces WHERE id='work'`).Scan(&storedCommit, &storedTitle); err != nil || storedCommit != merged["main_merge_commit"] || storedTitle != "Merge feature into main" {
		t.Fatalf("stored integration: %q %q, %v", storedCommit, storedTitle, err)
	}
}

func TestMainIntegrationNegativeLookupCache(t *testing.T) {
	useHost(t, "git-cache-host")
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Archive Test")
	git("config", "user.email", "archive@example.com")
	git("commit", "--allow-empty", "-m", "Base")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("checkout", "-b", "feature")
	git("commit", "--allow-empty", "-m", "Feature")
	head := git("rev-parse", "HEAD")

	catalog, _ := testCatalog(t)
	if _, err := catalog.DB.Exec(`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,head_ref,indexed_at)
		VALUES('work','conductor','local','source','Feature',?,?)`, head, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec(`INSERT INTO workspace_sightings(workspace_id,host_id,source_name,location,first_seen_at,last_seen_at)
		VALUES('work',?,'conductor',?,?,?)`, currentHost().ID, repo, now(), now()); err != nil {
		t.Fatal(err)
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "git-calls")
	shimDir := t.TempDir()
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$ALEXANDRIA_GIT_TRACE\"\nexec '" + strings.ReplaceAll(gitPath, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALEXANDRIA_GIT_TRACE", trace)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	refresh := func() []string {
		t.Helper()
		if err := os.WriteFile(trace, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := catalog.refreshMainIntegrations(context.Background()); err != nil {
			t.Fatal(err)
		}
		calls, err := os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(calls)), "\n")
	}
	if calls := refresh(); len(calls) < 3 {
		t.Fatalf("first negative scan made only %d Git calls: %v", len(calls), calls)
	}
	if calls := refresh(); len(calls) != 1 || !strings.Contains(calls[0], "rev-parse --verify refs/remotes/origin/main") {
		t.Fatalf("unchanged tip should need only one ref lookup, got %v", calls)
	}
	git("checkout", "main")
	git("commit", "--allow-empty", "-m", "More main work")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	if calls := refresh(); len(calls) < 3 {
		t.Fatalf("advanced main tip did not repeat ancestry check: %v", calls)
	}
	git("checkout", "feature")
	git("commit", "--allow-empty", "-m", "More feature work")
	newHead := git("rev-parse", "HEAD")
	if _, err := catalog.DB.Exec(`UPDATE workspaces SET head_ref=? WHERE id='work'`, newHead); err != nil {
		t.Fatal(err)
	}
	if calls := refresh(); len(calls) < 3 {
		t.Fatalf("changed workspace head did not repeat ancestry check: %v", calls)
	}
	git("checkout", "main")
	git("merge", "--no-ff", "feature", "-m", "Merge feature")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	refresh()
	var merged string
	if err := catalog.DB.QueryRow(`SELECT main_merge_commit FROM workspaces WHERE id='work'`).Scan(&merged); err != nil || merged == "" {
		t.Fatalf("merge after cached negatives was not recorded: %q, %v", merged, err)
	}
}
