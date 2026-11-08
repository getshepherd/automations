// automations: prompts that run on a schedule, each as an interactive agent in a terminal of Shepherd's backend
// (herdr, Rex, hc or tmux), in a new worktree or an existing folder. The model is t3code's scheduled tasks; the
// launch, the precheck and the run history are Orca's automations.
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	paddock "github.com/getshepherd/extensions"
)

//go:embed web
var web embed.FS

type Automation struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Prompt   string   `json:"prompt"`
	Enabled  bool     `json:"enabled"`
	Schedule Schedule `json:"schedule"`
	// Precheck is a shell command run in Dir before a scheduled run: exit 0 runs it, anything else skips it.
	Precheck string `json:"precheck,omitempty"`
	Timeout  int    `json:"timeout,omitempty"` // the precheck's, in seconds
	// Grace is how many minutes late a scheduled run may still start: later than that, it is recorded as missed and
	// the next is planned, so a laptop opened in the morning doesn't fire every run of the night. 0: 10.
	Grace    int       `json:"grace,omitempty"`
	Dir      string    `json:"dir"`
	Worktree bool      `json:"worktree"`          // a new worktree per run, branched from Base
	Base     string    `json:"base,omitempty"`    // empty: Dir's current branch
	Agent    string    `json:"agent,omitempty"`   // claude (default) or codex
	Backend  string    `json:"backend,omitempty"` // empty: the one Shepherd shows
	NextRun  time.Time `json:"next_run,omitzero"`
	LastRun  time.Time `json:"last_run,omitzero"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

func (a Automation) agent() string {
	if a.Agent == "" {
		return "claude"
	}
	return a.Agent
}

func (a Automation) base(dir string) string {
	if a.Base != "" {
		return a.Base
	}
	if out, err := run(dir, "git", "branch", "--show-current"); err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return "HEAD"
}

// Run is one firing. Status: started (the agent is in its terminal), skipped_precheck, skipped_missed (Shepherd's
// machine was asleep or the service down past the grace), failed.
type Run struct {
	ID         string    `json:"id"`
	Automation string    `json:"automation"`
	Trigger    string    `json:"trigger"` // scheduled or manual
	Status     string    `json:"status"`
	At         time.Time `json:"at"`
	Due        time.Time `json:"due,omitzero"`
	Place      *Place    `json:"place,omitempty"`
	Precheck   string    `json:"precheck,omitempty"` // its output, when it ran
	Error      string    `json:"error,omitempty"`
}

// missed says a due run is later than its grace allows.
func (a Automation) missed(now time.Time) bool {
	grace := a.Grace
	if grace <= 0 {
		grace = 10
	}
	return now.Sub(a.NextRun) > time.Duration(grace)*time.Minute
}

// keepRuns bounds each automation's history.
const keepRuns = 100

func main() {
	app := paddock.New("automations", "Prompts that run on a schedule, each as an agent in its own terminal", web)
	s := &store{dir: app.Dir()}

	paddock.Command(app, "list", "List the automations", func(_ *paddock.Ctx, _ struct{}) ([]Automation, error) {
		return s.automations()
	})
	paddock.Command(app, "show", "Show one automation, as JSON that save takes back", func(_ *paddock.Ctx, in struct {
		ID string `json:"id" help:"the automation's id, from list"`
	}) (Automation, error) {
		return s.get(in.ID)
	})
	paddock.Command(app, "save", "Add an automation, or replace one with --id: its name, prompt, folder and schedule (--kind interval|daily|once|cron)", func(c *paddock.Ctx, in saveInput) (Automation, error) {
		a, err := s.save(in, time.Now())
		if err == nil {
			c.Emit("changed", a.ID)
		}
		return a, err
	})
	paddock.Command(app, "enable", "Turn an automation on or off", func(c *paddock.Ctx, in struct {
		ID string `json:"id" help:"the automation's id"`
		On bool   `json:"on" help:"on; leave out to turn it off"`
	}) (Automation, error) {
		a, err := s.update(in.ID, func(a *Automation) {
			a.Enabled = in.On
			if in.On {
				a.NextRun = a.Schedule.Next(time.Now())
			}
		})
		if err == nil {
			c.Emit("changed", a.ID)
		}
		return a, err
	})
	paddock.Command(app, "remove", "Remove an automation and its history", func(c *paddock.Ctx, in struct {
		ID string `json:"id" help:"the automation's id"`
	}) (string, error) {
		err := s.remove(in.ID)
		if err == nil {
			c.Emit("changed", in.ID)
		}
		return in.ID, err
	})
	paddock.Command(app, "run", "Run an automation now, without its precheck", func(c *paddock.Ctx, in struct {
		ID string `json:"id" help:"the automation's id"`
	}) (Run, error) {
		a, err := s.get(in.ID)
		if err != nil {
			return Run{}, err
		}
		r := fire(s, a, "manual", time.Time{})
		c.Emit("changed", a.ID)
		return r, nil
	})
	paddock.Command(app, "runs", "An automation's runs, newest first (every automation's with no id)", func(_ *paddock.Ctx, in struct {
		ID string `json:"id" help:"the automation's id"`
	}) ([]Run, error) {
		return s.history(in.ID)
	})
	paddock.Command(app, "backend", "The backend runs go to when an automation names none: Shepherd's", func(_ *paddock.Ctx, _ struct{}) (string, error) {
		return shepherdBackend(), nil
	})

	paddock.Command(app, "projects", "The repositories the backend's terminals are in (Shepherd's backend with no --backend)", func(_ *paddock.Ctx, in struct {
		Backend string `json:"backend"`
	}) ([]Project, error) {
		return projects(in.Backend)
	})

	// ponytail: paddock has no hook for the service's own work, so the scheduler starts when the binary is the
	// service (`serve`); a paddock.App.OnServe when a second extension needs one
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		go schedule(s, app)
	}
	app.Run()
}

type saveInput struct {
	ID       string   `json:"id" help:"the automation to replace; leave out to add one"`
	Name     string   `json:"name" help:"what it is called"`
	Prompt   string   `json:"prompt" help:"the agent's first message"`
	Dir      string   `json:"dir" help:"the folder or repository it runs in"`
	Worktree bool     `json:"worktree" help:"a new worktree per run, branched from --base"`
	Base     string   `json:"base" help:"the branch a worktree starts from; default: the folder's current one"`
	Agent    string   `json:"agent" help:"claude (default) or codex"`
	Backend  string   `json:"backend" help:"herdr, rex, hc or tmux; default: the one Shepherd shows"`
	Kind     string   `json:"kind" help:"interval, daily, once or cron"`
	Every    int      `json:"every" help:"interval: minutes between runs"`
	At       string   `json:"at" help:"daily: HH:MM"`
	Days     []string `json:"days" help:"daily: weekdays, 0 (Sunday) to 6, or mon..sun; default: every day"`
	RunAt    string   `json:"run_at" help:"once: YYYY-MM-DD HH:MM, local time"`
	Cron     string   `json:"cron" help:"cron: minute hour day month weekday"`
	Precheck string   `json:"precheck" help:"a shell command run before a scheduled run; exit 0 runs it"`
	Timeout  int      `json:"timeout" help:"the precheck's time limit in seconds (default 60)"`
	Grace    int      `json:"grace" help:"how many minutes late a missed run may still start (default 10)"`
	Disabled bool     `json:"disabled" help:"save it turned off"`
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// day reads 0-6 (0 is Sunday) or a weekday's name, of which three letters are enough.
func day(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 6 {
		return n, nil
	}
	if len(s) >= 3 {
		if n := slices.Index(dayNames, s[:3]); n >= 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("day %q", s)
}

func (in saveInput) schedule() (Schedule, error) {
	s := Schedule{Kind: in.Kind, Every: in.Every, At: in.At, Cron: in.Cron}
	for _, d := range in.Days {
		for part := range strings.SplitSeq(d, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			from, to, isRange := strings.Cut(part, "-")
			a, err1 := day(from)
			b, err2 := a, error(nil)
			if isRange {
				b, err2 = day(to)
			}
			if err := errors.Join(err1, err2); err != nil || a > b {
				return s, fmt.Errorf("day %q: 0-6 or mon..sun, or a range of them (mon-fri)", part)
			}
			for n := a; n <= b; n++ {
				s.Weekdays = append(s.Weekdays, n)
			}
		}
	}
	if in.RunAt != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04", in.RunAt, time.Local)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, in.RunAt); err != nil {
				return s, fmt.Errorf("run_at %q: YYYY-MM-DD HH:MM", in.RunAt)
			}
		}
		s.RunAt = t
	}
	return s, s.Check()
}

// schedule fires what is due, each minute's runs at once.
func schedule(s *store, app *paddock.App) {
	for {
		now := time.Now()
		list, err := s.automations()
		if err != nil {
			fmt.Fprintln(os.Stderr, "automations:", err)
		}
		for _, a := range list {
			if !a.Enabled || a.NextRun.IsZero() || a.NextRun.After(now) {
				continue
			}
			due := a.NextRun
			if a.missed(now) {
				s.update(a.ID, func(a *Automation) { a.NextRun = a.Schedule.Next(now) })
				s.record(Run{ID: id(), Automation: a.ID, Trigger: "scheduled", Status: "skipped_missed", At: now, Due: due})
			} else if ok, out := precheck(a); !ok {
				s.update(a.ID, func(a *Automation) { a.NextRun = a.Schedule.Next(now) })
				s.record(Run{ID: id(), Automation: a.ID, Trigger: "scheduled", Status: "skipped_precheck", At: now, Due: due, Precheck: out})
			} else {
				fire(s, a, "scheduled", due)
			}
			app.Emit("changed", a.ID)
		}
		// wake on the next minute's start, as cron does
		time.Sleep(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)) + time.Second)
	}
}

// fire starts a run, plans the next and records it.
func fire(s *store, a Automation, trigger string, due time.Time) Run {
	now := time.Now()
	label := fmt.Sprintf("%s-%s", slug(a.Name), now.Format("0102-1504"))
	r := Run{ID: id(), Automation: a.ID, Trigger: trigger, Status: "started", At: now, Due: due}
	place, err := start(a, label)
	if place.Pane != "" || place.Dir != "" {
		r.Place = &place
	}
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	}
	s.update(a.ID, func(a *Automation) {
		a.LastRun = now
		if trigger == "scheduled" || a.Schedule.Kind == "interval" {
			a.NextRun = a.Schedule.Next(now)
		}
		if a.Schedule.Kind == "once" && trigger == "scheduled" {
			a.Enabled = false
		}
	})
	s.record(r)
	return r
}

func id() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// store keeps automations.json and runs.json in the extension's folder.
// ponytail: whole files read and written on each change; SQLite if there are ever thousands of runs
type store struct {
	dir string
	mu  sync.Mutex
}

func readJSON[T any](path string) ([]T, error) {
	list := []T{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return list, nil
	}
	if err != nil {
		return nil, err
	}
	return list, json.Unmarshal(b, &list)
}

func writeJSON[T any](path string, list []T) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *store) file(name string) string { return filepath.Join(s.dir, name) }

func (s *store) automations() ([]Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readJSON[Automation](s.file("automations.json"))
}

func (s *store) get(id string) (Automation, error) {
	list, err := s.automations()
	if err != nil {
		return Automation{}, err
	}
	if i := slices.IndexFunc(list, func(a Automation) bool { return a.ID == id }); i >= 0 {
		return list[i], nil
	}
	return Automation{}, fmt.Errorf("no automation %q", id)
}

func (s *store) change(fn func([]Automation) ([]Automation, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := readJSON[Automation](s.file("automations.json"))
	if err != nil {
		return err
	}
	if list, err = fn(list); err != nil {
		return err
	}
	return writeJSON(s.file("automations.json"), list)
}

func (s *store) update(id string, fn func(*Automation)) (Automation, error) {
	var out Automation
	err := s.change(func(list []Automation) ([]Automation, error) {
		i := slices.IndexFunc(list, func(a Automation) bool { return a.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("no automation %q", id)
		}
		fn(&list[i])
		list[i].Updated = time.Now()
		out = list[i]
		return list, nil
	})
	return out, err
}

func (s *store) save(in saveInput, now time.Time) (Automation, error) {
	sched, err := in.schedule()
	if err != nil {
		return Automation{}, err
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Prompt) == "" {
		return Automation{}, errors.New("name and prompt are needed")
	}
	if _, err := expand(in.Dir); err != nil {
		return Automation{}, err
	}
	if !slices.Contains([]string{"", "claude", "codex"}, in.Agent) {
		return Automation{}, fmt.Errorf("agent %q: claude or codex", in.Agent)
	}
	if !slices.Contains([]string{"", "herdr", "rex", "hc", "tmux"}, in.Backend) {
		return Automation{}, fmt.Errorf("backend %q: herdr, rex, hc or tmux", in.Backend)
	}
	a := Automation{
		ID: in.ID, Name: strings.TrimSpace(in.Name), Prompt: in.Prompt, Enabled: !in.Disabled, Schedule: sched,
		Precheck: in.Precheck, Timeout: in.Timeout, Grace: in.Grace, Dir: in.Dir, Worktree: in.Worktree, Base: in.Base,
		Agent: in.Agent, Backend: in.Backend, Created: now, Updated: now,
	}
	if a.Timeout == 0 {
		a.Timeout = 60
	}
	if a.Grace <= 0 {
		a.Grace = 10
	}
	if a.Enabled {
		a.NextRun = sched.Next(now)
	}
	err = s.change(func(list []Automation) ([]Automation, error) {
		if a.ID == "" {
			a.ID = id()
			return append(list, a), nil
		}
		i := slices.IndexFunc(list, func(old Automation) bool { return old.ID == a.ID })
		if i < 0 {
			return nil, fmt.Errorf("no automation %q", a.ID)
		}
		a.Created, a.LastRun = list[i].Created, list[i].LastRun
		list[i] = a
		return list, nil
	})
	return a, err
}

func (s *store) remove(id string) error {
	err := s.change(func(list []Automation) ([]Automation, error) {
		n := len(list)
		list = slices.DeleteFunc(list, func(a Automation) bool { return a.ID == id })
		if len(list) == n {
			return nil, fmt.Errorf("no automation %q", id)
		}
		return list, nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runs, err := readJSON[Run](s.file("runs.json"))
	if err != nil {
		return err
	}
	return writeJSON(s.file("runs.json"), slices.DeleteFunc(runs, func(r Run) bool { return r.Automation == id }))
}

func (s *store) record(r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs, err := readJSON[Run](s.file("runs.json"))
	if err != nil {
		return err
	}
	runs = append(runs, r)
	// the oldest beyond keepRuns of this automation go
	count := 0
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Automation == r.Automation {
			if count++; count > keepRuns {
				runs = slices.Delete(runs, i, i+1)
			}
		}
	}
	return writeJSON(s.file("runs.json"), runs)
}

func (s *store) history(id string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs, err := readJSON[Run](s.file("runs.json"))
	if err != nil {
		return nil, err
	}
	out := []Run{}
	for i := len(runs) - 1; i >= 0; i-- {
		if id == "" || runs[i].Automation == id {
			out = append(out, runs[i])
		}
	}
	return out, nil
}
