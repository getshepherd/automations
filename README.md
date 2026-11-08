# Automations

A Shepherd extension: prompts that run on a schedule, each as an interactive `claude` or `codex` in its own terminal
of the backend Shepherd shows (herdr, Rex, hc or tmux), where you see it and can take it over. A run starts in a new
worktree (branched from a base) or in a folder of yours. Built on
[paddock](https://github.com/getshepherd/extensions): one Go binary with its page, a CLI and a service.

- **When**: daily at HH:MM on chosen weekdays, every N minutes, once, or a cron line. A run the machine slept
  through by more than 10 minutes is recorded as missed, not fired late.
- **Precheck**: a shell command run in the folder before a scheduled run; exit 0 runs it, anything else skips it
  (`gh pr list --search review-requested:@me | grep -q .`). Run now skips the precheck.
- **History**: each run's status (started, skipped, missed, failed), where it went and the precheck's output; the
  last 100 per automation.

The model follows t3code's scheduled tasks; the launch, the precheck and the history follow Orca's automations.

## Install

```sh
go run . install --start shepherd   # runs while Shepherd is open; --start login keeps it running from login on
```

## CLI

```sh
automations save --name "Review PRs" --prompt "Review the PRs that ask for my review" --dir ~/code/app \
  --worktree --kind daily --at 09:30 --days mon-fri --precheck "gh pr list --search review-requested:@me | grep -q ."
automations list | show --id <id> | runs --id <id> | run --id <id> | enable --id <id> --on | remove --id <id>
```

`save --id <id>` replaces an automation with what it is given: `show` prints one as JSON to start from.

## How a run starts

The service reads the backend from Shepherd's `settings.json` (an automation may name another) and starts the agent
with that backend's CLI, its prompt as the first message: `herdr worktree create` or `workspace create` and then
`herdr agent start`; `rex new`; `tmux new-session`; `hc run --bg`. Outside herdr a worktree is made with
`git worktree add` where Shepherd puts its own, and the project's worktree hooks from Shepherd's `hooks.conf` run in
it. Claude Code's trust dialog in a new folder is answered yes: the folder is the one you picked.

Not yet: a run's end (it is recorded when the agent starts), webhooks, Codex's trust dialog.
