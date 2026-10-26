// ToDo monolith: server-rendered UI + JSON API on PostgreSQL, served over HTTPS.
// DB password and TLS cert are re-read from their mounted files, so secret rotation
// (External Secrets) and certificate renewal (cert-manager) apply without a restart.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type todo struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ToDo</title>
<style>body{font:16px system-ui;max-width:32rem;margin:2rem auto;padding:0 1rem}li{display:flex;gap:.5rem;align-items:center;margin:.3rem 0}
.done span{text-decoration:line-through;color:#888}form{display:inline}</style></head>
<body><h1>ToDo</h1>
<form method="post" action="/todos"><input name="title" placeholder="What needs doing?" required aria-label="New todo"> <button>Add</button></form>
<ul>{{range .}}<li class="{{if .Done}}done{{end}}" data-id="{{.ID}}">
<form method="post" action="/todos/{{.ID}}/toggle"><button aria-label="Toggle">{{if .Done}}↺{{else}}✓{{end}}</button></form>
<span>{{.Title}}</span>
<form method="post" action="/todos/{{.ID}}/delete"><button aria-label="Delete">✕</button></form></li>{{end}}</ul>
</body></html>`))

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		log.Printf("read %s: %v", p, err)
	}
	return strings.TrimSpace(string(b))
}

func main() {
	credDir := env("DB_CREDENTIALS_DIR", "/etc/db")
	tlsDir := env("TLS_DIR", "/etc/tls")
	connString := "host=" + env("DB_HOST", "todo-db-rw") + " dbname=" + env("DB_NAME", "todo") + " sslmode=require"

	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		log.Fatal(err)
	}
	// Fresh credentials on every new connection: rotated passwords are picked up without a restart.
	cfg.BeforeConnect = func(_ context.Context, c *pgx.ConnConfig) error {
		c.User = readFile(credDir + "/username")
		c.Password = readFile(credDir + "/password")
		return nil
	}
	cfg.MaxConnLifetime = time.Minute
	ctx := context.Background()
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	for {
		_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS todos (
			id bigserial PRIMARY KEY, title text NOT NULL, done boolean NOT NULL DEFAULT false,
			created_at timestamptz NOT NULL DEFAULT now())`)
		if err == nil {
			break
		}
		log.Printf("waiting for database: %v", err)
		time.Sleep(3 * time.Second)
	}

	list := func(ctx context.Context) ([]todo, error) {
		rows, err := db.Query(ctx, `SELECT id, title, done FROM todos ORDER BY id`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByPos[todo])
	}
	add := func(ctx context.Context, title string) (id int64, err error) {
		err = db.QueryRow(ctx, `INSERT INTO todos (title) VALUES ($1) RETURNING id`, title).Scan(&id)
		return
	}
	fail := func(w http.ResponseWriter, err error) {
		log.Print(err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
	back := func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusSeeOther) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		todos, err := list(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		page.Execute(w, todos)
	})
	mux.HandleFunc("POST /todos", func(w http.ResponseWriter, r *http.Request) {
		if title := strings.TrimSpace(r.FormValue("title")); title != "" {
			if _, err := add(r.Context(), title); err != nil {
				fail(w, err)
				return
			}
		}
		back(w, r)
	})
	mux.HandleFunc("POST /todos/{id}/toggle", func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.Exec(r.Context(), `UPDATE todos SET done = NOT done WHERE id = $1`, r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		back(w, r)
	})
	mux.HandleFunc("POST /todos/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.Exec(r.Context(), `DELETE FROM todos WHERE id = $1`, r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		back(w, r)
	})
	mux.HandleFunc("GET /api/todos", func(w http.ResponseWriter, r *http.Request) {
		todos, err := list(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(todos)
	})
	mux.HandleFunc("POST /api/todos", func(w http.ResponseWriter, r *http.Request) {
		var in todo
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Title) == "" {
			http.Error(w, "title required", http.StatusBadRequest)
			return
		}
		id, err := add(r.Context(), in.Title)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(todo{ID: id, Title: in.Title})
	})
	// Opens a brand-new connection with the credentials currently on disk, so it fails
	// if a rotated password has not reached both the Secret mount and PostgreSQL.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fresh := cfg.ConnConfig.Copy()
		cfg.BeforeConnect(r.Context(), fresh)
		conn, err := pgx.ConnectConfig(r.Context(), fresh)
		if err != nil {
			fail(w, err)
			return
		}
		conn.Close(r.Context())
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(env("APP_VERSION", "dev") + "\n"))
	})

	srv := &http.Server{
		Addr:    ":" + env("PORT", "8443"),
		Handler: mux,
		TLSConfig: &tls.Config{
			// ponytail: reloads the key pair on every handshake; cache by mtime if traffic ever matters.
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				c, err := tls.LoadX509KeyPair(tlsDir+"/tls.crt", tlsDir+"/tls.key")
				return &c, err
			},
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s", srv.Addr)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
