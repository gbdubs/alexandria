package archive

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// repositoryFromLocation avoids confusing a worktree or branch directory with a repository.
func repositoryFromLocation(location, remote string) map[string]any {
	return repositoryAt(location, remote, true)
}

// repositoryFromName names the repository without asking Git, for a location
// recorded on another Mac.
func repositoryFromName(location, remote string) map[string]any {
	return repositoryAt(location, remote, false)
}

func repositoryAt(location, remote string, lookup bool) map[string]any {
	root := ""
	if info, err := os.Stat(location); lookup && location != "" && err == nil && info.IsDir() {
		git := func(args ...string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "git", append([]string{"-C", location}, args...)...).Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(output))
		}
		if remote == "" {
			remote = git("config", "--get", "remote.origin.url")
		}
		if common := git("rev-parse", "--path-format=absolute", "--git-common-dir"); common != "" {
			root = filepath.Dir(common)
		}
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(location)), "/")
	layout := ""
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "conductor" && parts[i+1] == "workspaces" {
			layout = parts[i+2]
		}
	}
	if filepath.Base(filepath.Dir(location)) == ".conductor" {
		layout = filepath.Base(filepath.Dir(filepath.Dir(location)))
	}
	name := ""
	if remote != "" {
		name = strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
		if i := strings.LastIndexAny(name, "/:"); i >= 0 {
			name = name[i+1:]
		}
	}
	if name == "" && root != "" {
		name = filepath.Base(root)
	}
	if name == "" {
		name = layout
	}
	if name == "" {
		return nil
	}
	locations := uniqueStrings([]string{root, location})
	return map[string]any{"display_name": name, "canonical_remote": nilIfEmpty(remote), "local_locations": locations}
}
