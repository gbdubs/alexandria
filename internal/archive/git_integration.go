package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
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
	tip, err := gitOutput(repository, "rev-parse", "--verify", "refs/remotes/origin/main^{commit}")
	if err != nil {
		return nil
	}
	integration, _ := mainIntegrationAtTip(repository, head, remote, tip)
	return integration
}

func gitOutput(repository string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...).Output()
	return strings.TrimSpace(string(output)), err
}

// checked says Git determined ancestry conclusively. A nil integration with
// checked=true can be cached; missing objects and failed commands cannot.
func mainIntegrationAtTip(repository, head, remote, tip string) (map[string]any, bool) {
	if !gitCommitID.MatchString(head) || !gitCommitID.MatchString(tip) {
		return nil, false
	}
	head, err := gitOutput(repository, "rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return nil, false
	}
	if _, err := gitOutput(repository, "merge-base", "--is-ancestor", head, tip); err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			// A shallow boundary can make an ancestor appear absent without
			// changing the remote tip when the checkout is later deepened.
			shallow, err := gitOutput(repository, "rev-parse", "--is-shallow-repository")
			return nil, err == nil && shallow == "false"
		}
		return nil, false
	}
	lineage, err := gitOutput(repository, "rev-list", "--first-parent", "--reverse", tip)
	if err != nil || lineage == "" {
		return nil, false
	}
	commits := strings.Split(lineage, "\n")
	low, high := 0, len(commits)-1
	for low < high {
		middle := low + (high-low)/2
		if _, err := gitOutput(repository, "merge-base", "--is-ancestor", head, commits[middle]); err == nil {
			high = middle
		} else {
			low = middle + 1
		}
	}
	commit := commits[low]
	title, err := gitOutput(repository, "show", "-s", "--format=%s", commit)
	if err != nil {
		return nil, false
	}
	parents, err := gitOutput(repository, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, false
	}
	method := "direct"
	if len(strings.Fields(parents)) > 2 {
		method = "merge"
	}
	result := map[string]any{"main_merge_commit": commit, "main_merge_title": title, "main_merge_method": method}
	if slug := githubSlug(remote); slug != "" {
		result["main_merge_url"] = "https://github.com/" + slug + "/commit/" + commit
	}
	return result, true
}

// refreshMainIntegrations only runs Git in paths this host recorded. Another
// Mac's path may not exist here, or may be an unrelated checkout: Conductor
// reuses workspace directory names.
func (c *Catalog) refreshMainIntegrations(ctx context.Context) error {
	c.gitMainMu.Lock()
	defer c.gitMainMu.Unlock()
	rows, err := queryMaps(c.DB, `SELECT w.id,w.head_ref,r.canonical_remote,s.location,s.repository_locations_json
		FROM workspaces w JOIN workspace_sightings s ON s.workspace_id=w.id AND s.host_id=?
		LEFT JOIN repositories r ON r.id=w.repository_id
		WHERE w.head_ref IS NOT NULL AND w.main_merge_commit IS NULL ORDER BY w.id,s.source_name`, currentHost().ID)
	if err != nil {
		return err
	}
	negativeRows, err := queryMaps(c.DB, `SELECT workspace_id,location,head_ref,main_tip FROM git_main_negative_lookups WHERE host_id=?`, currentHost().ID)
	if err != nil {
		return err
	}
	negative := make(map[string]map[string]any, len(negativeRows))
	for _, row := range negativeRows {
		negative[firstString(row["workspace_id"])+"\x00"+firstString(row["location"])] = row
	}
	// A workspace can have several local paths, but one path needs only one
	// ref lookup in this pass even if several workspaces refer to it.
	tips := map[string]string{}
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
			if location == "" {
				continue
			}
			tip, seen := tips[location]
			if !seen {
				if info, err := os.Stat(location); err == nil && info.IsDir() {
					tip, _ = gitOutput(location, "rev-parse", "--verify", "refs/remotes/origin/main^{commit}")
				}
				tips[location] = tip
			}
			if tip == "" {
				continue
			}
			head := firstString(row["head_ref"])
			if cached := negative[firstString(row["id"])+"\x00"+location]; cached != nil &&
				firstString(cached["head_ref"]) == head && firstString(cached["main_tip"]) == tip {
				continue
			}
			integration, checked := mainIntegrationAtTip(location, head, firstString(row["canonical_remote"]), tip)
			if integration == nil {
				if checked {
					if _, err := c.DB.Exec(`INSERT INTO git_main_negative_lookups(workspace_id,host_id,location,head_ref,main_tip,checked_at)
						VALUES(?,?,?,?,?,?) ON CONFLICT(workspace_id,host_id,location) DO UPDATE SET
						head_ref=excluded.head_ref,main_tip=excluded.main_tip,checked_at=excluded.checked_at`,
						row["id"], currentHost().ID, location, head, tip, now()); err != nil {
						return err
					}
					negative[firstString(row["id"])+"\x00"+location] = map[string]any{"head_ref": head, "main_tip": tip}
				}
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
