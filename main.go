package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed ui.html assets
var uiFS embed.FS

const statuses = "backlog,plan_ready,fixing,review,done"

var (
	db     *sql.DB
	apiTok string
	subs   = map[chan string]struct{}{}
)

func dataDir() string {
	if d := os.Getenv("KANBAN_DATA"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local/share/kanban")
}

func attachDir() string {
	d := filepath.Join(dataDir(), "attachments")
	os.MkdirAll(d, 0o755)
	return d
}

func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" {
		h, _ := os.UserHomeDir()
		return h
	}
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~/"))
	}
	return p
}

func mustDB() string {
	dir := dataDir()
	os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "kanban.db")
}

func token() string {
	p := filepath.Join(dataDir(), "token")
	if b, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(b))
	}
	b := make([]byte, 18)
	for i := range b {
		b[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[time.Now().UnixNano()%36]
		b[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[(int(time.Now().UnixNano()>>uint(i*3))+i*7)%36]
	}
	t := string(b)
	os.WriteFile(p, []byte(t), 0o600)
	return t
}

func openDB() *sql.DB {
	d, err := sql.Open("sqlite", mustDB()+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		panic(err)
	}
	schema := `
CREATE TABLE IF NOT EXISTS apps (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  source TEXT DEFAULT 'manual', repo_path TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS issues (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  title TEXT NOT NULL, description TEXT DEFAULT '', plan TEXT DEFAULT '',
  review_notes TEXT DEFAULT '', status TEXT DEFAULT 'backlog',
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS attachments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  filename TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP);`
	if _, err := d.Exec(schema); err != nil {
		panic(err)
	}
	return d
}

type App struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	RepoPath string `json:"repo_path"`
}

type Issue struct {
	ID          int64  `json:"id"`
	AppID       string `json:"app_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Plan        string `json:"plan"`
	ReviewNotes string `json:"review_notes"`
	Status      string `json:"status"`
	UpdatedAt   string `json:"updated_at"`
}

func query(q string, args ...any) []map[string]any {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		m := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = vals[i]
			}
		}
		out = append(out, m)
	}
	return out
}

// ---- docker discovery ----
// composeRoots maps compose project name -> host folder holding its compose file.
func composeRoots() map[string]string {
	out := map[string]string{}
	cmd := exec.Command("docker", "compose", "ls", "--format", "json")
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+":/usr/bin:/bin")
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	var rows []struct {
		Name        string   `json:"Name"`
		ConfigFiles string   `json:"ConfigFiles"`
	}
	if json.Unmarshal(b, &rows) != nil {
		return out
	}
	for _, r := range rows {
		if r.ConfigFiles == "" {
			continue
		}
		files := strings.Split(r.ConfigFiles, ",")
		out[r.Name] = filepath.Dir(strings.TrimSpace(files[0]))
	}
	return out
}

func syncDocker(prefix string) []string {
	roots := composeRoots()
	cmd := exec.Command("docker", "ps", "--format", `{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Names}}`)
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+":/usr/bin:/bin")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var added []string
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		name := parts[0]
		if name == "" {
			name = parts[1]
		}
		id := strings.NewReplacer("-", "_").Replace(strings.ToLower(name))
		if seen[id] {
			continue
		}
		seen[id] = true
		res, err := db.Exec(`INSERT INTO apps (id,name,source,repo_path) VALUES (?,?,'docker',?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name,
			repo_path=CASE WHEN excluded.repo_path<>'' THEN excluded.repo_path ELSE apps.repo_path END`,
			prefix+id, name, roots[name])
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added = append(added, name)
		}
	}
	return added
}

// syncProjects registers every git repo under root as an app pointing at its real folder.
func syncProjects(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var added []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(root, e.Name())
		src := "folder"
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			src = "repo"
		}
		id := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(e.Name()))
		var existing string
		var present int
		db.QueryRow(`SELECT repo_path, COUNT(*) FROM apps WHERE id=?`, id).Scan(&existing, &present)
		if existing != "" && existing != path {
			continue
		}
		_, err := db.Exec(`INSERT INTO apps (id,name,source,repo_path) VALUES (?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET repo_path=excluded.repo_path,
			name=excluded.name, source=excluded.source`, id, e.Name(), src, path)
		if err == nil && present == 0 {
			added = append(added, e.Name())
		}
	}
	return added
}

// ---- HTTP ----
func auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := r.Header.Get("X-Kanban-Token")
		if t == "" {
			t = r.URL.Query().Get("token")
		}
		if t == "" {
			if c, err := r.Cookie("kanban_token"); err == nil {
				t = c.Value
			}
		}
		if t != apiTok {
			http.Error(w, "unauthorized", 401)
			return
		}
		h(w, r)
	}
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func notify() {
	for c := range subs {
		select {
		case c <- "update":
		default:
		}
	}
}

func handleAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/apps", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, query(`SELECT id,name,source,COALESCE(repo_path,'') repo_path
			FROM apps ORDER BY name`))
	})
	mux.HandleFunc("POST /api/apps", func(w http.ResponseWriter, r *http.Request) {
		var a App
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if strings.TrimSpace(a.ID) == "" {
			http.Error(w, "id required", 400)
			return
		}
		_, err := db.Exec(`INSERT OR REPLACE INTO apps (id,name,source,repo_path)
			VALUES (?,?,COALESCE(?, 'manual'),?)`, a.ID, a.Name, a.Source, expandHome(a.RepoPath))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		jsonOut(w, a)
	})
	mux.HandleFunc("PATCH /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kanban-Human") != "1" {
			http.Error(w, "harness may not edit apps", http.StatusForbidden)
			return
		}
		id := r.PathValue("id")
		var a App
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name := strings.TrimSpace(a.Name)
		if name == "" {
			http.Error(w, "name required", 400)
			return
		}
		res, err := db.Exec(`UPDATE apps SET name=?, repo_path=? WHERE id=?`, name, expandHome(a.RepoPath), id)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "no such app", 404)
			return
		}
		notify()
		jsonOut(w, query(`SELECT id,name,source,COALESCE(repo_path,'') repo_path FROM apps WHERE id=?`, id))
	})
	mux.HandleFunc("GET /api/apps/{id}/issues", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, query(`SELECT id,app_id,title,description,plan,review_notes,status,updated_at
			FROM issues WHERE app_id=? ORDER BY id DESC`, r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/apps/{id}/issues", func(w http.ResponseWriter, r *http.Request) {
		var i Issue
		if err := json.NewDecoder(r.Body).Decode(&i); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		jsonOut(w, addIssue(r.PathValue("id"), i))
	})
	mux.HandleFunc("PATCH /api/issues/{id}", func(w http.ResponseWriter, r *http.Request) {
		var i Issue
		if err := json.NewDecoder(r.Body).Decode(&i); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		human := r.Header.Get("X-Kanban-Human") == "1"
		updated, err := patchIssue(r.PathValue("id"), i, human)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		jsonOut(w, updated)
	})
	mux.HandleFunc("GET /api/counts", func(w http.ResponseWriter, r *http.Request) {
		// per-app counts by lane, plus the open total the sidebar badge shows
		counts := map[string]map[string]int64{}
		for _, row := range query(`SELECT app_id, status, COUNT(*) n FROM issues
			GROUP BY app_id, status`) {
			id, st := fmt.Sprint(row["app_id"]), fmt.Sprint(row["status"])
			if counts[id] == nil {
				counts[id] = map[string]int64{}
			}
			counts[id][st] = scanInt(row["n"])
		}
		for _, c := range counts {
			open := int64(0)
			for _, st := range []string{"backlog", "plan_ready", "fixing", "review"} {
				open += c[st]
			}
			c["open"] = open
		}
		jsonOut(w, counts)
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		h, _ := os.UserHomeDir()
		projects := syncProjects(filepath.Join(h, "projects"))
		docker := syncDocker("docker_")
		notify()
		jsonOut(w, map[string]any{"projects": projects, "docker": docker})
	})
	mux.HandleFunc("DELETE /api/issues/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kanban-Human") != "1" {
			http.Error(w, "harness may not delete issues", http.StatusForbidden)
			return
		}
		issueID := r.PathValue("id")
		for _, row := range query(`SELECT id, filename FROM attachments WHERE issue_id=?`, issueID) {
			ext := filepath.Ext(fmt.Sprint(row["filename"]))
			if ext == "" {
				ext = ".jpg"
			}
			os.Remove(filepath.Join(attachDir(), fmt.Sprintf("%v%s", row["id"], ext)))
		}
		if _, err := db.Exec(`DELETE FROM issues WHERE id=?`, issueID); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		notify()
		jsonOut(w, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("GET /api/issues/{id}/attachments", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, query(`SELECT id, issue_id, filename, mime, size, created_at
			FROM attachments WHERE issue_id=? ORDER BY id ASC`, r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/issues/{id}/attachments", func(w http.ResponseWriter, r *http.Request) {
		issueID := r.PathValue("id")
		r.Body = http.MaxBytesReader(w, r.Body, 15<<20)
		if err := r.ParseMultipartForm(15 << 20); err != nil {
			http.Error(w, "file too large (max 15MB) or invalid multipart", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		buf := make([]byte, 512)
		n, _ := file.Read(buf)
		mime := http.DetectContentType(buf[:n])
		if ct := header.Header.Get("Content-Type"); ct != "" && strings.HasPrefix(ct, "image/") {
			mime = ct
		}
		if !strings.HasPrefix(mime, "image/") {
			http.Error(w, "only image attachments are supported", http.StatusBadRequest)
			return
		}

		cleanName := filepath.Base(header.Filename)
		if cleanName == "" || cleanName == "." {
			cleanName = "photo.jpg"
		}
		ext := filepath.Ext(cleanName)
		if ext == "" {
			ext = ".jpg"
		}

		res, err := db.Exec(`INSERT INTO attachments (issue_id, filename, mime, size) VALUES (?, ?, ?, ?)`,
			issueID, cleanName, mime, header.Size)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		attID, _ := res.LastInsertId()
		dstPath := filepath.Join(attachDir(), fmt.Sprintf("%d%s", attID, ext))
		dst, err := os.Create(dstPath)
		if err != nil {
			db.Exec(`DELETE FROM attachments WHERE id=?`, attID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			dst.Close()
			db.Exec(`DELETE FROM attachments WHERE id=?`, attID)
			os.Remove(dstPath)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		copied, err := io.Copy(dst, file)
		dst.Close()
		if err != nil {
			db.Exec(`DELETE FROM attachments WHERE id=?`, attID)
			os.Remove(dstPath)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		totalSize := int64(n) + copied
		db.Exec(`UPDATE attachments SET size=? WHERE id=?`, totalSize, attID)
		notify()
		jsonOut(w, map[string]any{"id": attID, "filename": cleanName, "size": totalSize, "mime": mime})
	})
	mux.HandleFunc("GET /api/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rows := query(`SELECT id, filename, mime FROM attachments WHERE id=?`, id)
		if len(rows) == 0 {
			http.Error(w, "attachment not found", http.StatusNotFound)
			return
		}
		filename := fmt.Sprint(rows[0]["filename"])
		mime := fmt.Sprint(rows[0]["mime"])
		ext := filepath.Ext(filename)
		if ext == "" {
			ext = ".jpg"
		}
		diskPath := filepath.Join(attachDir(), fmt.Sprintf("%s%s", id, ext))
		if _, err := os.Stat(diskPath); os.IsNotExist(err) {
			http.Error(w, "attachment file missing", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, diskPath)
	})
	mux.HandleFunc("DELETE /api/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kanban-Human") != "1" {
			http.Error(w, "harness may not delete attachments", http.StatusForbidden)
			return
		}
		id := r.PathValue("id")
		rows := query(`SELECT filename FROM attachments WHERE id=?`, id)
		if len(rows) > 0 {
			filename := fmt.Sprint(rows[0]["filename"])
			ext := filepath.Ext(filename)
			if ext == "" {
				ext = ".jpg"
			}
			os.Remove(filepath.Join(attachDir(), fmt.Sprintf("%s%s", id, ext)))
		}
		db.Exec(`DELETE FROM attachments WHERE id=?`, id)
		notify()
		jsonOut(w, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("DELETE /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kanban-Human") != "1" {
			http.Error(w, "harness may not delete apps", http.StatusForbidden)
			return
		}
		id := r.PathValue("id")
		if _, err := db.Exec(`DELETE FROM issues WHERE app_id=?`, id); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if _, err := db.Exec(`DELETE FROM apps WHERE id=?`, id); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		notify()
		jsonOut(w, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		c := make(chan string, 4)
		subs[c] = struct{}{}
		defer delete(subs, c)
		for {
			select {
			case msg := <-c:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				f.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	return auth(mux.ServeHTTP)
}

func addIssue(appID string, i Issue) Issue {
	st := i.Status
	if st == "" {
		st = "backlog"
	}
	res, err := db.Exec(`INSERT INTO issues (app_id,title,description,plan,status)
		VALUES (?,?,?,?,?)`, appID, i.Title, i.Description, i.Plan, st)
	if err != nil {
		return Issue{AppID: appID, Title: "error: " + err.Error()}
	}
	id, _ := res.LastInsertId()
	notify()
	rows := query(`SELECT id,app_id,title,description,plan,review_notes,status,updated_at
		FROM issues WHERE id=?`, id)
	return scanIssue(rows[0])
}

func scanInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		return n
	}
}

func scanIssue(m map[string]any) Issue {
	toInt := scanInt
	toStr := func(v any) string {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	return Issue{
		ID:          toInt(m["id"]),
		AppID:       toStr(m["app_id"]),
		Title:       toStr(m["title"]),
		Description: toStr(m["description"]),
		Plan:        toStr(m["plan"]),
		ReviewNotes: toStr(m["review_notes"]),
		Status:      toStr(m["status"]),
		UpdatedAt:   toStr(m["updated_at"]),
	}
}

// patchIssue enforces the role boundary: harness may move up to 'review'; only a human sets 'done'.
func patchIssue(id string, patch Issue, human bool) (Issue, error) {
	rows := query(`SELECT id,app_id,title,description,plan,review_notes,status,updated_at FROM issues WHERE id=?`, id)
	if len(rows) == 0 {
		return Issue{}, fmt.Errorf("issue %s not found", id)
	}
	cur := scanIssue(rows[0])
	if patch.Status != "" {
		if !strings.Contains(","+statuses+",", ","+patch.Status+",") {
			return Issue{}, fmt.Errorf("invalid status %q", patch.Status)
		}
		if patch.Status == "done" && !human {
			return Issue{}, fmt.Errorf("harness may not move to done; human approval required")
		}
		cur.Status = patch.Status
	}
	if patch.Title != "" {
		cur.Title = patch.Title
	}
	if patch.Description != "" {
		cur.Description = patch.Description
	}
	if patch.Plan != "" {
		cur.Plan = patch.Plan
	}
	if patch.ReviewNotes != "" {
		cur.ReviewNotes = patch.ReviewNotes
	}
	_, err := db.Exec(`UPDATE issues SET title=?,description=?,plan=?,review_notes=?,status=?,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		cur.Title, cur.Description, cur.Plan, cur.ReviewNotes, cur.Status, cur.ID)
	if err != nil {
		return Issue{}, err
	}
	notify()
	return cur, nil
}

func mimeByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webmanifest":
		return "application/manifest+json"
	}
	return "application/octet-stream"
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	page, _ := uiFS.ReadFile("ui.html")
	cookie := &http.Cookie{Name: "kanban_token", Value: apiTok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	http.SetCookie(w, cookie)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, strings.Replace(string(page), "{{TOKEN}}", apiTok, 1))
}

// stripPrefix lets the board live under a reverse-proxy path (e.g. Tailscale serve /kanban)
// whether or not the proxy strips the prefix itself.
func stripPrefix(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/kanban/") || r.URL.Path == "/kanban" {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/kanban")
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}
		h.ServeHTTP(w, r)
	})
}

func serve(listen string) {
	db = openDB()
	apiTok = token()
	mux := http.NewServeMux()
	// static assets: icon, PWA manifest (paths are also reachable under a proxy prefix)
	for _, f := range []string{"manifest.webmanifest", "icon.svg", "icon-192.png", "icon-512.png",
		"icon-maskable.png", "apple-touch-icon.png"} {
		name := f
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) {
			b, err := uiFS.ReadFile("assets/" + name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", mimeByExt(name))
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(b)
		})
	}
	mux.Handle("/api/", handleAPI())
	mux.HandleFunc("/", handleUI)
	srv := &http.Server{Addr: listen, Handler: stripPrefix(mux), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("kanban board on http://%s  (db %s)\n", listen, mustDB())
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---- CLI ----
func cli(args []string) {
	db = openDB()
	if len(args) == 0 {
		usage()
		return
	}
	switch args[0] {
	case "triage":
		// Everything a planning pass needs: apps that have unscoped backlog items.
		var out []map[string]any
		for _, a := range query(`SELECT id,name,COALESCE(repo_path,'') repo_path FROM apps ORDER BY name`) {
			issues := query(`SELECT id,title,description,status FROM issues
				WHERE app_id=? AND status='backlog' ORDER BY id`, a["id"])
			if len(issues) == 0 {
				continue
			}
			a["issues"] = issues
			out = append(out, a)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	case "app":
		switch args[1] {
		case "add":
			name, repo := args[2], ""
			if len(args) > 3 {
				repo = args[3]
			}
			id := strings.NewReplacer("-", "_").Replace(strings.ToLower(name))
			db.Exec(`INSERT OR REPLACE INTO apps (id,name,source,repo_path) VALUES (?,?,'manual',?)`, id, name, expandHome(repo))
			fmt.Println(id)
		case "sync-projects":
			root := filepath.Join(os.Getenv("HOME"), "projects")
			if len(args) > 2 {
				root = args[2]
			}
			added := syncProjects(root)
			fmt.Printf("linked %d project(s): %v\n", len(added), added)
		case "sync-docker":
			prefix := "docker_"
			if len(args) > 2 {
				prefix = args[2]
			}
			added := syncDocker(prefix)
			fmt.Printf("imported %d app(s): %v\n", len(added), added)
		case "list":
			for _, a := range query(`SELECT id,name,source FROM apps ORDER BY name`) {
				fmt.Printf("%-24s %-8s %v\n", a["id"], a["source"], a["name"])
			}
		case "rm":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: kanban app rm <app_id>")
				return
			}
			id := args[2]
			db.Exec(`DELETE FROM issues WHERE app_id=?`, id)
			res, err := db.Exec(`DELETE FROM apps WHERE id=?`, id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				fmt.Fprintf(os.Stderr, "no such app: %s\n", id)
				return
			}
			fmt.Printf("removed %s\n", id)
		}
	case "issue":
		sub := args[1]
		switch sub {
		case "add":
			appID, title := args[2], args[3]
			var plan, desc string
			for i := 4; i < len(args); i++ {
				a := args[i]
				switch {
				case a == "--plan" && i+1 < len(args):
					plan = args[i+1]
					i++
				case strings.HasPrefix(a, "--plan="):
					plan = strings.TrimPrefix(a, "--plan=")
				case a == "--desc" && i+1 < len(args):
					desc = args[i+1]
					i++
				case strings.HasPrefix(a, "--desc="):
					desc = strings.TrimPrefix(a, "--desc=")
				case !strings.HasPrefix(a, "-") && plan == "":
					plan = a
				}
			}
			st := "backlog"
			if plan != "" {
				st = "plan_ready"
			}
			i := addIssue(appID, Issue{Title: title, Description: desc, Plan: plan, Status: st})
			if i.Title == "" {
				fmt.Println(`{"error":"insert failed"}`)
				return
			}
			b, _ := json.Marshal(i)
			fmt.Println(string(b))
		case "list":
			appID := args[2]
			st := ""
			if len(args) > 4 {
				st = args[4]
			}
			rows := query(`SELECT id,title,status,updated_at FROM issues WHERE app_id=? ORDER BY id`, appID)
			var out []map[string]any
			for _, r := range rows {
				if st == "" || fmt.Sprint(r["status"]) == st {
					out = append(out, r)
				}
			}
			b, _ := json.Marshal(out)
			fmt.Println(string(b))
		case "next":
			appID := args[2]
			rows := query(`SELECT id,app_id,title,description,plan,review_notes,status,updated_at
				FROM issues WHERE app_id=? AND status='plan_ready' ORDER BY id LIMIT 1`, appID)
			if len(rows) == 0 {
				fmt.Println(`{"error":"no plan_ready issue"}`)
				return
			}
			i := scanIssue(rows[0])
			if _, err := patchIssue(fmt.Sprint(i.ID), Issue{Status: "fixing"}, false); err != nil {
				fmt.Printf("{\"error\":%q}\n", err.Error())
				return
			}
			i.Status = "fixing"
			b, _ := json.Marshal(i)
			fmt.Println(string(b))
		case "move":
			id := args[2]
			human := false
			status := ""
			for _, a := range args[3:] {
				if a == "--human" {
					human = true
				} else if !strings.HasPrefix(a, "-") {
					status = a
				}
			}
			if _, err := patchIssue(id, Issue{Status: status}, human); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("ok")
		case "plan":
			id := args[2]
			plan := strings.Join(args[3:], " ")
			if plan == "" {
				fmt.Fprintln(os.Stderr, "usage: kanban issue plan <id> \"<plan text>\"")
				return
			}
			if _, err := patchIssue(id, Issue{Plan: plan, Status: "plan_ready"}, false); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("ok")
		case "review":
			id := args[2]
			notes := ""
			for i, a := range args {
				if a == "--notes" && i+1 < len(args) {
					notes = args[i+1]
				} else if strings.HasPrefix(a, "--notes=") {
					notes = strings.TrimPrefix(a, "--notes=")
				}
			}
			_, err := patchIssue(id, Issue{Status: "review", ReviewNotes: notes}, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("ok")
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `kanban - local board

  kanban serve [--listen 0.0.0.0:3456]
  kanban app add <name> [repo_path]
  kanban app rm <app_id>
  kanban app sync-projects [root]    # link every git repo under ~/projects
  kanban app sync-docker [id_prefix]
  kanban app list
  kanban triage                    # JSON: every app with backlog items
  kanban issue add <app> <title> [--plan "..."] [--desc "..."]
  kanban issue list <app> [--status <status>]
  kanban issue next <app>          # plan_ready -> fixing
  kanban issue plan <id> "<plan text>"    # write plan, move to plan_ready
  kanban issue review <id> --notes "..."  # -> review
  kanban issue move <id> <status> [--human]
`)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		listen := fs.String("listen", "0.0.0.0:3456", "listen address")
		fs.Parse(os.Args[2:])
		serve(*listen)
		return
	}
	cli(os.Args[1:])
}
