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
