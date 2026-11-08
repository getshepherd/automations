package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	paddock "github.com/getshepherd/extensions"
)

// A run's agent is an interactive `claude` or `codex` in a terminal of Shepherd's backend, where the user sees it
// and can take it over, started with that backend's own CLI: the same commands the `shepherd` agent skill teaches.

// Place is where a run's agent went, in the backend's own ids.
type Place struct {
	Backend string `json:"backend"`
	Session string `json:"session,omitempty"` // Rex's session, tmux's session, herdr's workspace
	Pane    string `json:"pane"`              // Rex's block, tmux's pane, herdr's pane, hc's terminal
	Dir     string `json:"dir"`
	Branch  string `json:"branch,omitempty"`
}

// shepherdBackend is the backend Shepherd shows, from its settings beside the extensions folder.
func shepherdBackend() string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(paddock.ExtensionsDir()), "settings.json"))
	var s struct {
		Backend string `json:"backend"`
	}
	if err != nil || json.Unmarshal(b, &s) != nil || s.Backend == "" {
		return "herdr" // Shepherd's default
	}
	return s.Backend
}

var loginPath sync.Once

// usePATH gives the service the login shell's PATH: launchd and systemd start it with a bare one, where neither the
// agents nor the backends' CLIs are.
func usePATH() {
	loginPath.Do(func() {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, shell, "-l", "-c", `printf %s "$PATH"`).Output()
		if err == nil && len(out) > 0 {
			os.Setenv("PATH", string(out))
		}
	})
}

func run(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args[:min(len(args), 2)], " "), err, msg)
	}
	return out.String(), nil
}

// field reads one value out of a command's JSON with a dotted path, as jq's `.a.b[0].c`.
func field(out, path string) string {
	var v any
	if json.Unmarshal([]byte(out), &v) != nil {
		return ""
	}
	for _, key := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		name, index, list := strings.Cut(strings.TrimSuffix(key, "]"), "[")
		if m, ok := v.(map[string]any); ok {
			v = m[name]
		} else {
			return ""
		}
		if list {
			a, ok := v.([]any)
			if !ok || len(a) == 0 || index != "0" {
				return ""
			}
			v = a[0]
		}
	}
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "run"
	}
	return s
}

// start puts a's run in a terminal: in a new worktree if a.Worktree, else in a.Dir.
func start(a Automation, label string) (Place, error) {
	usePATH()
	backend := a.Backend
	if backend == "" {
		backend = shepherdBackend()
	}
	dir, err := expand(a.Dir)
	if err != nil {
		return Place{}, err
	}
	p := Place{Backend: backend, Dir: dir}
	argv := []string{a.agent(), a.Prompt}

	if a.Worktree {
		p.Branch = "auto/" + label
		if backend == "herdr" {
			out, err := run(dir, "herdr", "worktree", "create", "--cwd", dir, "--branch", p.Branch, "--base", a.base(dir), "--label", a.Name, "--no-focus")
			if err != nil {
				return p, err
			}
			p.Session, p.Pane = field(out, ".result.workspace.workspace_id"), field(out, ".result.root_pane.pane_id")
			// ponytail: the checkout's path as `worktree list` names it; unchecked against a live create
			if path := field(out, ".result.worktree.path"); path != "" {
				p.Dir = path
			}
			return p, herdrAgent(p, label, a.agent(), a.Prompt)
		}
		if p.Dir, err = worktree(dir, p.Branch, a.base(dir)); err != nil {
			return p, err
		}
	}

	switch backend {
	case "herdr":
		out, err := run(p.Dir, "herdr", "workspace", "create", "--cwd", p.Dir, "--label", a.Name, "--no-focus")
		if err != nil {
			return p, err
		}
		p.Session, p.Pane = field(out, ".result.workspace.workspace_id"), field(out, ".result.root_pane.pane_id")
		return p, herdrAgent(p, label, a.agent(), a.Prompt)
	case "rex":
		out, err := run(p.Dir, "rex", append([]string{"new", a.Name, "-c", p.Dir, "--json", "--"}, argv...)...)
		if err != nil {
			return p, err
		}
		p.Session, p.Pane = field(out, ".session_id"), field(out, ".initial_windows[0].block_ids[0]")
	case "tmux":
		out, err := run(p.Dir, "tmux", append([]string{"new-session", "-d", "-P", "-F", "#{session_id} #{pane_id}", "-s", label, "-c", p.Dir}, argv...)...)
		if err != nil {
			return p, err
		}
		p.Session, p.Pane, _ = strings.Cut(strings.TrimSpace(out), " ")
	case "hc":
		out, err := run(p.Dir, "hc", append([]string{"run", "--bg", "--json", "--no-parent", "--name", label, "--cwd", p.Dir}, argv...)...)
		if err != nil {
			return p, err
		}
		p.Pane = field(out, ".address")
		if p.Pane == "" {
			p.Pane = label
		}
	default:
		return p, fmt.Errorf("backend %q: herdr, rex, hc or tmux", backend)
	}
	go answerTrust(p)
	return p, nil
}

// herdrAgent starts the agent with herdr's own agent start, which waits until it is ready, the prompt as its first.
func herdrAgent(p Place, name, kind, prompt string) error {
	_, err := run(p.Dir, "herdr", "agent", "start", name, "--kind", kind, "--pane", p.Pane, "--timeout", "60000", "--", prompt)
	if err != nil && strings.Contains(err.Error(), "agent_not_ready") {
		go answerTrust(p) // blocked at startup: most likely the trust dialog of a folder it hasn't seen
		return nil
	}
	return err
}

// trustDialog is Claude Code's question in a folder it hasn't opened before; nothing runs until it is answered.
const trustDialog = "Is this a project you created or one you trust"

// answerTrust says yes to Claude Code's trust dialog when it shows within the first minute. The automation's folder is
// the user's own choice, which is the trust the dialog asks for.
// ponytail: Claude Code's dialog only; Codex's asks something else and waits for the user.
func answerTrust(p Place) {
	for range 30 {
		time.Sleep(2 * time.Second)
		screen, err := read(p)
		if err != nil {
			return
		}
		if strings.Contains(screen, trustDialog) {
			keys(p, "down", "enter")
			return
		}
		if strings.Contains(screen, "❯") && !strings.Contains(screen, "trust") {
			return // past it, at the prompt
		}
	}
}

func read(p Place) (string, error) {
	switch p.Backend {
	case "herdr":
		return run(p.Dir, "herdr", "pane", "read", p.Pane, "--source", "recent-unwrapped", "--lines", "60")
	case "rex":
		return run(p.Dir, "rex", "capture", "-s", p.Session, "-b", p.Pane, "--trim")
	case "tmux":
		return run(p.Dir, "tmux", "capture-pane", "-p", "-J", "-t", p.Pane)
	case "hc":
		return run(p.Dir, "hc", "read", p.Pane)
	}
	return "", errors.New("no backend")
}

func keys(p Place, ks ...string) {
	switch p.Backend {
	case "herdr":
		run(p.Dir, "herdr", append([]string{"pane", "send-keys", p.Pane}, ks...)...)
	case "rex":
		for _, k := range ks {
			run(p.Dir, "rex", "send-key", "-s", p.Session, "-b", p.Pane, k)
		}
	case "tmux":
		tm := map[string]string{"down": "Down", "enter": "Enter"}
		for _, k := range ks {
			run(p.Dir, "tmux", "send-keys", "-t", p.Pane, tm[k])
		}
	case "hc":
		run(p.Dir, "hc", append([]string{"keys", p.Pane}, ks...)...)
	}
}

func expand(dir string) (string, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("dir %q: an absolute path", dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("dir %q: no such folder", dir)
	}
	return dir, nil
}

// worktree checks branch out in a new folder where Shepherd puts its own (herdr's `worktrees.directory`, else
// ~/.herdr/worktrees/<repo>/<branch>), then runs the project's worktree hooks, as Shepherd does for those it makes.
func worktree(dir, branch, base string) (string, error) {
	common, err := run(dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s: not a git repository: %w", dir, err)
	}
	root := filepath.Dir(strings.TrimSpace(common))
	home, _ := os.UserHomeDir()
	// ponytail: the default folder only; a `worktrees.directory` in herdr's config.toml isn't read yet
	path := filepath.Join(home, ".herdr", "worktrees", filepath.Base(root), slug(branch))
	if _, err := run(root, "git", "worktree", "add", "-b", branch, path, base); err != nil {
		return "", err
	}
	if err := hooks(root, path); err != nil {
		return path, fmt.Errorf("worktree hooks: %w", err)
	}
	return path, nil
}

// hooks runs the `created =` lines of root's project in Shepherd's hooks.conf, in order, the first failure stopping
// the rest: the format `shepherd hooks` writes and Shepherd's herdr plugin reads.
func hooks(root, path string) error {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		home, _ := os.UserHomeDir()
		config = filepath.Join(home, ".config")
	}
	f, err := os.Open(filepath.Join(config, "herdr", "plugins", "config", "shepherd.worktree-hooks", "hooks.conf"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	home, _ := os.UserHomeDir()
	var project string
	var mine bool
	var lines []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSuffix(scan.Text(), "\r")
		if section, ok := strings.CutPrefix(line, "["); ok && strings.HasSuffix(section, "]") {
			_, name, found := strings.Cut(strings.TrimSuffix(section, "]"), "/")
			if !found {
				name = strings.TrimSuffix(section, "]")
			}
			project, mine = name, false
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		value = strings.TrimLeft(value, " ")
		switch strings.TrimSpace(key) {
		case "dir":
			d := value
			if d == "~" || strings.HasPrefix(d, "~/") {
				d = filepath.Join(home, strings.TrimPrefix(d, "~"))
			}
			mine = filepath.Clean(d) == root
		case "created":
			if mine {
				lines = append(lines, value)
			}
		}
	}
	for _, line := range lines {
		cmd := exec.Command("/bin/sh", "-c", line)
		cmd.Dir = path
		cmd.Env = append(os.Environ(), "ROOT_PATH="+root, "WORKTREE_PATH="+path, "PROJECT="+project, "EVENT=created")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", line, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// precheck runs the automation's check in its folder: exit 0 lets the run go.
func precheck(a Automation) (bool, string) {
	if strings.TrimSpace(a.Precheck) == "" {
		return true, ""
	}
	usePATH()
	dir, err := expand(a.Dir)
	if err != nil {
		return false, err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(max(a.Timeout, 1))*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", a.Precheck)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 2000 {
		text = text[len(text)-2000:]
	}
	if ctx.Err() != nil {
		return false, fmt.Sprintf("timed out after %ds\n%s", a.Timeout, text)
	}
	return err == nil, text
}

// Project is a repository one of the backend's terminals is in, for the form's project picker.
type Project struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// projects are the repositories the backend's terminals are in, each once: a worktree counts as the repository it
// belongs to, and a folder outside git is left out (the form takes any other folder by its path).
func projects(backend string) ([]Project, error) {
	usePATH()
	if backend == "" {
		backend = shepherdBackend()
	}
	var dirs []string
	switch backend {
	case "herdr":
		out, err := run("", "herdr", "workspace", "list")
		if err != nil {
			return nil, err
		}
		var r struct {
			Result struct {
				Workspaces []struct {
					Worktree struct {
						RepoRoot string `json:"repo_root"`
					} `json:"worktree"`
				} `json:"workspaces"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			return nil, err
		}
		for _, w := range r.Result.Workspaces {
			dirs = append(dirs, w.Worktree.RepoRoot)
		}
	case "rex":
		// ponytail: a session's folder is its first window's focused block's, as Shepherd names a Rex workspace;
		// two calls a session, side by side
		out, err := run("", "rex", "list", "--json")
		if err != nil {
			return nil, err
		}
		var l struct {
			Sessions []struct {
				ID string `json:"session_id"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			return nil, err
		}
		dirs = make([]string, len(l.Sessions))
		var wg sync.WaitGroup
		for i, s := range l.Sessions {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, _ := run("", "rex", "session", "inspect", s.ID, "--json")
				if block := field(out, "windows[0].focused_block_id"); block != "" {
					out, _ := run("", "rex", "block", "call", "-s", s.ID, "-b", block, "process")
					dirs[i] = field(out, "foreground.cwd")
				}
			}()
		}
		wg.Wait()
	case "tmux":
		out, err := run("", "tmux", "list-panes", "-a", "-F", "#{pane_current_path}")
		if err != nil && (strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting")) {
			return []Project{}, nil // no server: nothing open
		}
		if err != nil {
			return nil, err
		}
		dirs = strings.Split(out, "\n")
	case "hc":
		out, err := run("", "hc", "ls", "--json")
		if err != nil {
			return nil, err
		}
		var l struct {
			Terminals []struct {
				Cwd        string `json:"cwd"`
				Foreground struct {
					Cwd string `json:"cwd"`
				} `json:"foreground"`
			} `json:"terminals"`
		}
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			return nil, err
		}
		for _, t := range l.Terminals {
			dirs = append(dirs, cmp.Or(t.Foreground.Cwd, t.Cwd))
		}
	default:
		return nil, fmt.Errorf("unknown backend %q", backend)
	}
	seen := map[string]bool{}
	list := []Project{}
	for _, d := range dirs {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		// the common git dir is the main checkout's .git, for a worktree too
		out, err := run(d, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
		root := filepath.Dir(strings.TrimSpace(out))
		if err != nil || filepath.Base(strings.TrimSpace(out)) != ".git" || seen[root] {
			continue
		}
		seen[root] = true
		list = append(list, Project{Name: filepath.Base(root), Dir: root})
	}
	slices.SortFunc(list, func(a, b Project) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return list, nil
}
