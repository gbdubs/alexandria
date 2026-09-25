package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A GUI app, and the service it starts, inherit launchd's environment rather
// than the login shell's, yet the agents a user starts from Terminal honor
// variables such as CLAUDE_CONFIG_DIR exported by shell startup files.

const shellEnvTimeout = 2 * time.Second

var shellEnvCache struct {
	sync.Mutex
	values map[string]string
	err    error
	at     time.Time
}

// cachedLoginShellEnv returns nil values and no error when disabled with
// PHAROS_PROBE_SHELL=0.
func cachedLoginShellEnv(names []string) (map[string]string, error) {
	if os.Getenv("PHAROS_PROBE_SHELL") == "0" {
		return nil, nil
	}
	shellEnvCache.Lock()
	defer shellEnvCache.Unlock()
	if !shellEnvCache.at.IsZero() && time.Since(shellEnvCache.at) < 10*time.Minute {
		return shellEnvCache.values, shellEnvCache.err
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	shellEnvCache.values, shellEnvCache.err = loginShellEnv(shell, names, shellEnvTimeout)
	shellEnvCache.at = time.Now()
	return shellEnvCache.values, shellEnvCache.err
}

// loginShellEnv prints names from an interactive login shell between random
// markers, tolerating whatever its startup files print. The shell runs in a
// new session without a terminal, so it cannot stop on terminal I/O, and its
// whole process group is killed at the deadline.
func loginShellEnv(shell string, names []string, timeout time.Duration) (map[string]string, error) {
	marker := "__PHAROS_ENV_" + strings.NewReplacer("-", "", "_", "").Replace(randomToken())
	script := "printf '\\n%s\\n' " + marker
	for _, name := range names {
		script += "; printf '%s\\n' \"$" + name + "\""
	}
	script += "; printf '%s\\n' " + marker + "_END"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, shell, "-l", "-i", "-c", script)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	// Background jobs started by startup files may hold stdout open.
	command.WaitDelay = 250 * time.Millisecond
	output, err := command.Output()
	text := strings.ReplaceAll(string(output), "\r", "")
	lines := []string{}
	if start := strings.Index(text, "\n"+marker+"\n"); start >= 0 {
		lines = strings.Split(text[start+len(marker)+2:], "\n")
	}
	if len(lines) <= len(names) || lines[len(names)] != marker+"_END" {
		switch {
		case ctx.Err() != nil:
			return nil, fmt.Errorf("%s did not finish within %s", shell, timeout)
		case err != nil:
			return nil, fmt.Errorf("%s: %w", shell, err)
		}
		return nil, errors.New("the login shell's output was not understood")
	}
	values := map[string]string{}
	for index, name := range names {
		if value := strings.TrimSpace(lines[index]); value != "" {
			values[name] = value
		}
	}
	return values, nil
}
