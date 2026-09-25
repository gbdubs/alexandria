package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The initial Git ancestry scan can take minutes on a large catalog. Keep it
// out of OpenCatalog so the local API can become ready before it runs.
func (c *Catalog) backfillMainIntegrations(ctx context.Context) error {
	var completed string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key='git_main_backfill'").Scan(&completed); err == nil {
		return nil
	} else if err != sql.ErrNoRows {
		return err
	}
	if err := c.refreshMainIntegrations(ctx); err != nil {
		return err
	}
	_, err := c.DB.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('git_main_backfill','1')")
	return err
}

var gitCommitID = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)

// mainIntegration uses only local Git objects. A squash merge cannot be
// established by commit ancestry, so it is deliberately left unclassified.
func mainIntegration(repository, head, remote string) map[string]any {
	if repository == "" || !gitCommitID.MatchString(head) {
		return nil
	}
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...).Output()
		return strings.TrimSpace(string(output)), err
	}
	if _, err := git("show-ref", "--verify", "--quiet", "refs/remotes/origin/main"); err != nil {
		return nil
	}
	head, err := git("rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return nil
	}
	if _, err := git("merge-base", "--is-ancestor", head, "refs/remotes/origin/main"); err != nil {
		return nil
	}
	lineage, err := git("rev-list", "--first-parent", "--reverse", "refs/remotes/origin/main")
	if err != nil || lineage == "" {
		return nil
	}
	commits := strings.Split(lineage, "\n")
	low, high := 0, len(commits)-1
	for low < high {
		middle := low + (high-low)/2
		if _, err := git("merge-base", "--is-ancestor", head, commits[middle]); err == nil {
			high = middle
		} else {
			low = middle + 1
		}
	}
	commit := commits[low]
	title, err := git("show", "-s", "--format=%s", commit)
	if err != nil {
		return nil
	}
	parents, err := git("rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil
	}
	method := "direct"
	if len(strings.Fields(parents)) > 2 {
		method = "merge"
	}
	result := map[string]any{"main_merge_commit": commit, "main_merge_title": title, "main_merge_method": method}
	if slug := githubSlug(remote); slug != "" {
		result["main_merge_url"] = "https://github.com/" + slug + "/commit/" + commit
	}
	return result
}

// refreshMainIntegrations only runs Git in paths this host recorded. Another
// Mac's path may not exist here, or may be an unrelated checkout: Conductor
// reuses workspace directory names.
func (c *Catalog) refreshMainIntegrations(ctx context.Context) error {
	rows, err := queryMaps(c.DB, `SELECT w.id,w.head_ref,r.canonical_remote,s.location,s.repository_locations_json
		FROM workspaces w JOIN workspace_sightings s ON s.workspace_id=w.id AND s.host_id=?
		LEFT JOIN repositories r ON r.id=w.repository_id
		WHERE w.head_ref IS NOT NULL AND w.main_merge_commit IS NULL ORDER BY w.id,s.source_name`, currentHost().ID)
	if err != nil {
		return err
	}
	for start := 0; start < len(rows); {
		row := rows[start]
		var locations []string
		for start < len(rows) && rows[start]["id"] == row["id"] {
			var repository []string
			_ = json.Unmarshal([]byte(firstString(rows[start]["repository_locations_json"])), &repository)
			locations = append(append(locations, repository...), firstString(rows[start]["location"]))
			start++
		}
		for _, location := range uniqueStrings(locations) {
			if err := ctx.Err(); err != nil {
				return err
			}
			integration := mainIntegration(location, firstString(row["head_ref"]), firstString(row["canonical_remote"]))
			if integration == nil {
				continue
			}
			_, err := c.DB.Exec(`UPDATE workspaces SET main_merge_commit=?,main_merge_title=?,main_merge_url=?,main_merge_method=? WHERE id=?`,
				integration["main_merge_commit"], integration["main_merge_title"], integration["main_merge_url"], integration["main_merge_method"], row["id"])
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}
