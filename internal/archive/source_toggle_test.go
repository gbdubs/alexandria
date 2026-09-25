package archive

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Run with -race: toggles used to write into the Sources array that every
// Config() copy shares, while other requests iterated it.
func TestSourceTogglesAreRaceFreeAndNotLost(t *testing.T) {
	probeHome(t)
	useHost(t, "host-a")
	names := []string{"s0", "s1", "s2", "s3", "s4", "s5"}
	blocks := ""
	for _, name := range names {
		blocks += fmt.Sprintf("\n[[sources]]\nname = %q\nkind = \"claude\"\npath = \"/nowhere/%s\"\nenabled = true\n", name, name)
	}
	path := probeLibrary(t, blocks)
	server := probeServer(t, path)
	var wait sync.WaitGroup
	for _, name := range names {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for round := range 6 {
				body := fmt.Sprintf(`{"enabled":%t}`, round%2 == 1)
				if code, response := probeCall(t, server, http.MethodPost, "/api/sources/"+name+"/enabled", body); code != 200 {
					t.Errorf("toggle %s: %d %v", name, code, response)
				}
			}
		}()
	}
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 30 {
				enabled := 0
				for _, source := range server.Config().Sources {
					if source.Enabled {
						enabled++
					}
				}
				_ = enabled
				if code, response := probeCall(t, server, http.MethodGet, "/api/sources", ""); code != 200 {
					t.Errorf("sources: %d %v", code, response)
				}
			}
		}()
	}
	wait.Wait()
	// Every toggle ended on enabled = true (round 5); none may be lost in
	// the running configuration or on disk.
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []Config{server.Config(), reloaded} {
		for _, source := range config.Sources {
			if !source.Enabled {
				t.Errorf("%s ended disabled", source.Name)
			}
		}
	}
	if text := probeReadText(t, path); strings.Contains(text, "enabled = false") {
		t.Errorf("library.toml = %s", text)
	}
}

// A toggle may not undo a probe accept's reload of the running sources, and
// neither may drop the other's write to the host file.
func TestSourceTogglesAndProbeAcceptsKeepEachOthersChanges(t *testing.T) {
	home := probeHome(t)
	useHost(t, "host-a")
	profiles := []string{}
	for index := range 6 {
		name := fmt.Sprintf("claude-p%d", index)
		profiles = append(profiles, name)
		probePut(t, filepath.Join(home, "."+name, "projects", "-r", fmt.Sprintf("00000000-0000-4000-8000-%012d.jsonl", index)), "{}\n")
	}
	path := probeLibrary(t, "")
	host := filepath.Join(filepath.Dir(path), "hosts", "host-a.toml")
	probePut(t, host, "[[sources]]\nname = \"base\"\nkind = \"claude\"\npath = \"/nowhere\"\nenabled = true\n")
	server := probeServer(t, path)
	var wait sync.WaitGroup
	for _, name := range profiles {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if code, response := probeCall(t, server, http.MethodPost, "/api/probe/accept", fmt.Sprintf(`{"accept":[%q]}`, name)); code != 200 {
				t.Errorf("accept %s: %d %v", name, code, response)
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		for round := range 20 {
			body := fmt.Sprintf(`{"enabled":%t}`, round%2 == 1)
			if code, response := probeCall(t, server, http.MethodPost, "/api/sources/base/enabled", body); code != 200 {
				t.Errorf("toggle: %d %v", code, response)
			}
		}
	}()
	wait.Wait()
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for label, config := range map[string]Config{"running": server.Config(), "on disk": reloaded} {
		enabled := map[string]bool{}
		for _, source := range config.Sources {
			enabled[source.Name] = source.Enabled
		}
		if len(enabled) != len(profiles)+1 || !enabled["base"] {
			t.Errorf("%s sources: %v", label, enabled)
		}
		for _, name := range profiles {
			if !enabled[name] {
				t.Errorf("%s: accepted %s is missing or disabled", label, name)
			}
		}
	}
}
