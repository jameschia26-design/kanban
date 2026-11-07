# kanban

Local issue board for every app under `~/projects`. One board per app, five columns:
**Backlog → Plan Ready → Fixing → Review → Done**.

The board is for the human. Coding agents use the same data through the CLI or HTTP API and may
only move work as far as **Review** — closing an issue is a human action.

- Board: http://100.86.17.86:3456 (Tailscale) or http://127.0.0.1:3456
- Data: `~/.local/share/kanban/kanban.db` (SQLite + WAL, outside the repo so rebuilds never touch it)
- Service: `systemctl --user status kanban`
- Agent skill: `~/.agents/skills/kanban/SKILL.md` (symlinked into `~/.hermes/skills` and `~/.claude/skills`)

## Build and run

```bash
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/kanban .
systemctl --user restart kanban
```

Single Go binary, `modernc.org/sqlite` (no cgo), UI embedded from `ui.html`.

## CLI

```bash
kanban app list
kanban app sync-projects              # link every folder in ~/projects
kanban app sync-docker                # import running docker stacks
kanban app add <name> [repo_path]

kanban issue list <app> [--status plan_ready]
kanban issue add <app> "<title>" --plan "..." --desc "..."
kanban issue next <app>               # plan_ready -> fixing
kanban issue review <id> --notes "..."
kanban issue move <id> <status> [--human]
```

`kanban issue move <id> done` fails unless `--human` is passed, mirroring the API gate.

## API

Header `X-Kanban-Token` (value in `~/.local/share/kanban/token`) is required; the board UI also
sends `X-Kanban-Human: 1`, which is what unlocks `done`, `DELETE`, and manual edits.

```
GET    /api/apps
POST   /api/apps
GET    /api/apps/{id}/issues
POST   /api/apps/{id}/issues
PATCH  /api/issues/{id}
DELETE /api/issues/{id}
GET    /api/counts
POST   /api/sync
GET    /api/events                    (SSE, pushes board changes to open tabs)
```

## Tests

```bash
go test ./...     # move gate + project linking
```

## Notes

- Column moves by drag-drop in the UI; agents cannot reorder into `done`.
- `triage.sh` writes plans for backlog items on demand. No schedule is installed; run it by hand.
- Token lives beside the database, not in git.
- Ponytail: no auth beyond the token, no multi-user roles. Add a reverse-proxy auth layer (or
  Tailscale-only binding) if the board ever leaves the tailnet.

## Look and feel

- Brand mark `assets/icon.svg` (three kanban lanes); PNGs are generated from it with headless Chrome.
  Regenerate: `cd assets && google-chrome --headless --screenshot=icon-512.png --window-size=512,512 src/tile.svg`
- Installable as a PWA: `manifest.webmanifest`, 192/512 icons, maskable icon, apple-touch icon.
- Light/dark toggle in the sidebar footer; first visit follows the OS, choice is remembered.
- Sidebar order = open (non-done) issue count, highest first, empty projects last, ties alphabetical.
- Pencil on a sidebar row edits the project name and folder; agents cannot edit apps, only humans.
