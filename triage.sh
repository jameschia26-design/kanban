#!/bin/sh
# Nightly triage: turn raw backlog items into scoped plans. Read-only on code.
# Runs the Pi harness headless with the kanban skill. Timer: kanban-triage.timer
set -eu
export PATH="$HOME/.local/bin:$PATH"
export KANBAN_DATA="${KANBAN_DATA:-$HOME/.local/share/kanban}"

PROMPT='You are the scheduled kanban triage run. Work only on planning, never on code.

1. Run `kanban triage`. It prints JSON: every app that has backlog items, with its repo_path and the issues.
2. For each issue, look at that repo read-only (ls, rg, read files) long enough to understand the area it touches.
3. Write a scoped plan: which files to touch, the approach, risks, and how to verify it. 3-8 lines, concrete, no filler.
4. Save each plan exactly like this: kanban issue plan <id> "<plan text>"
   That also moves the issue to Plan Ready, which is the hand-off point for a working session.
5. If an issue is too vague to plan, leave it in Backlog and say why in your summary.

Hard limits: do not edit application code, do not create branches, do not commit, do not push, do not deploy, do not move anything to Fixing, Review or Done. This run only writes plans.

Finish with a two-line summary: how many plans written, how many skipped.'

exec timeout 1800 pi -p --no-session \
  --skill "$HOME/.agents/skills/kanban/SKILL.md" \
  "$PROMPT"
