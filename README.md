# Kanban

A fast, lightweight, single-binary issue board designed for humans collaborating with autonomous coding agents (Claude Code, Pi, Hermes, Cursor).

One board per project, five-stage pipeline:
**Backlog → Plan Ready → Fixing → Review → Done**.

The core premise: **agents do the work, humans hold the gate**. Agents may pick up planned tasks, move them to `Fixing`, and submit them for `Review`. Only a human can mark an issue `Done` or delete issues.

---

## Highlights

- **Single Go binary**: Pure Go with `modernc.org/sqlite` (zero CGO). The entire UI and brand assets are embedded via `embed.FS`.
- **Safe data isolation**: SQLite database, tokens, and image attachments live outside the repository in `~/.local/share/kanban` (or `$KANBAN_DATA`). Updating code or pulling git commits never touches your data.
- **Remote & mobile ready**: Listens on `0.0.0.0:3456` for easy access over Tailscale or LAN. Includes a mobile slide-out drawer with touch targets and auto-authenticating session cookies.
- **Photo attachments with client-side auto-resize**: Paste screenshots from clipboard, drag-drop images, or upload files. Large camera shots (>2MB or >1920px) are auto-downscaled in the browser via canvas before upload to save mobile bandwidth.
- **Live updates**: Real-time board synchronization via Server-Sent Events (SSE).
- **Auto-discovery**: Instantly link git repositories in `~/projects` or active Docker Compose stacks with one CLI command.

---

## Build & Install

Prerequisite: Go 1.22+

```bash
# Clone
git clone https://github.com/<your-username>/kanban.git
cd kanban

# Build binary
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/kanban .

# Run directly
kanban serve --listen 0.0.0.0:3456
```

### Systemd User Service (Auto-start on boot)

To keep the board running in the background and auto-restart on boot:

Create `~/.config/systemd/user/kanban.service`:

```ini
[Unit]
Description=Kanban board for local apps
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/kanban serve --listen 0.0.0.0:3456
Environment=KANBAN_DATA=%h/.local/share/kanban
Restart=always
RestartSec=3
MemoryMax=192M
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=default.target
```

Enable and start:
```bash
systemctl --user daemon-reload
systemctl --user enable --now kanban.service
loginctl enable-linger $USER   # ensures service runs on boot before login
```

---

## CLI Reference

```bash
# Manage Apps
kanban app list
kanban app sync-projects [path]       # scan and link git repos (default: ~/projects)
kanban app sync-docker [prefix]       # import running docker compose stacks
kanban app add <name> [repo_path]     # manually register an app
kanban app rm <app_id>                # unregister an app

# Manage Issues
kanban issue list <app> [--status plan_ready]
kanban issue add <app> "<title>" [--plan "..."] [--desc "..."]
kanban issue next <app>               # claims oldest plan_ready -> fixing
kanban issue plan <id> "<plan text>"  # records plan -> moves to plan_ready
kanban issue review <id> --notes "..."# records work notes -> moves to review
kanban issue move <id> <status> [--human]
```

*Note: `kanban issue move <id> done` rejects agent callers and requires `--human`.*

---

## Coding Agent Integration (Harness Skill)

To teach your agent (Claude Code, Pi, Hermes) how to interact with the board, expose a skill or rule:

```markdown
# Kanban Skill

- Find work: `kanban issue list <app> --status plan_ready`
- Claim task: `kanban issue next <app>` (sets status to 'fixing')
- Implement: Make changes inside the app's repo folder.
- Submit for verification: `kanban issue review <id> --notes "<what was changed and verified>"`
- Never mark done: Closing issues is human-only.
```

---

## HTTP API

All endpoints require the `X-Kanban-Token` header (or cookie `kanban_token`). Value is generated once in `~/.local/share/kanban/token`.

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/apps` | List all tracked apps |
| `POST` | `/api/apps` | Register or update an app |
| `GET` | `/api/apps/{id}/issues` | List issues for an app |
| `POST` | `/api/apps/{id}/issues` | Create new issue |
| `PATCH` | `/api/issues/{id}` | Update issue (status, plan, notes). Setting `done` requires `X-Kanban-Human: 1` |
| `DELETE` | `/api/issues/{id}` | Delete issue (requires `X-Kanban-Human: 1`) |
| `GET` | `/api/issues/{id}/attachments` | List image attachments |
| `POST` | `/api/issues/{id}/attachments` | Upload image (multipart/form-data) |
| `GET` | `/api/attachments/{id}` | Serve image file |
| `DELETE` | `/api/attachments/{id}` | Delete image (requires `X-Kanban-Human: 1`) |
| `GET` | `/api/counts` | Issue counts per lane across all apps |
| `GET` | `/api/events` | SSE feed for real-time UI updates |

---

## License

[MIT](LICENSE)
