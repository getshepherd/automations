Use it when the user wants an agent to do something later or again and again: "every morning, review the PRs waiting
on me", "check CI every hour and fix it if it's red", "tomorrow at 10, update the dependencies". Each automation is a
prompt that starts as a new agent in its own terminal on schedule, in a fresh worktree or in the project folder.

- Write the prompt for an agent that starts with nothing else: the commands to start from, what not to touch, and what
  to end with. A run costs tokens: when most runs would find nothing to do, add a `--precheck`, a shell command run in
  the folder first, whose exit 0 lets the run start.
- Show the user the automation you made (`list`), with its next run. Change one with `show`, then `save --id`.
- `runs` says how the last runs went; `run` starts one now to try it.
