package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type App struct {
	db   *sql.DB
	tmpl map[string]*template.Template
	loc  *time.Location
	hash string // hash pbkdf2 de la contraseña
	ver  string // versión de assets (hash del css/js) para cache busting
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
    id            smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    savings_goal  bigint NOT NULL DEFAULT 1500000,
    weekly_cash   bigint NOT NULL DEFAULT 100000
);
INSERT INTO settings (id) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS templates (
    id       serial PRIMARY KEY,
    kind     text NOT NULL CHECK (kind IN ('income','fixed')),
    name     text NOT NULL,
    amount   bigint NOT NULL,
    active   boolean NOT NULL DEFAULT true,
    position smallint NOT NULL DEFAULT 0,
    pay_cash boolean NOT NULL DEFAULT false
);
-- migración para bases existentes: fijos que se pagan en efectivo (del sobre)
ALTER TABLE templates ADD COLUMN IF NOT EXISTS pay_cash boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS cycles (
    id         serial PRIMARY KEY,
    started_on date NOT NULL,
    closed_on  date
);

CREATE TABLE IF NOT EXISTS transactions (
    id          serial PRIMARY KEY,
    cycle_id    int NOT NULL REFERENCES cycles(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('card','cash','withdrawal','income','fixed','cardpay','loan_out','loan_in')),
    template_id int REFERENCES templates(id) ON DELETE SET NULL,
    amount      bigint NOT NULL,
    concept     text NOT NULL DEFAULT '',
    category    text NOT NULL DEFAULT '',
    credit      boolean NOT NULL DEFAULT false,
    cash        boolean NOT NULL DEFAULT false,
    made_on     date NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    client_id   text UNIQUE
);
CREATE INDEX IF NOT EXISTS transactions_cycle_idx ON transactions (cycle_id, kind);
-- migración para bases existentes: rubros (comida, libros, alcohol)
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS category text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS transactions_cat_idx ON transactions (cycle_id, category);
-- migración para bases existentes: nuevo tipo 'cardpay' (pago de la tarjeta
-- de crédito, no cuenta como gasto). Se recrea el CHECK con la lista completa.
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_kind_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_kind_check
    CHECK (kind IN ('card','cash','withdrawal','income','fixed','cardpay','loan_out','loan_in'));
-- migración para bases existentes: entradas en efectivo (van al sobre semanal)
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS cash boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS transactions_made_idx ON transactions (made_on);

-- migración para bases existentes: idempotencia de la cola offline (un
-- reintento de sincronización con el mismo client_id nunca duplica)
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS client_id text UNIQUE;

CREATE TABLE IF NOT EXISTS todo_cats (
    id       serial PRIMARY KEY,
    name     text NOT NULL,
    position smallint NOT NULL DEFAULT 0
);
INSERT INTO todo_cats (name) SELECT 'General' WHERE NOT EXISTS (SELECT 1 FROM todo_cats);

CREATE TABLE IF NOT EXISTS todos (
    id         serial PRIMARY KEY,
    client_id  text UNIQUE,
    body       text NOT NULL,
    cat_id     int REFERENCES todo_cats(id),
    done_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
-- migración para bases existentes: pendientes sin categoría van a la primera
ALTER TABLE todos ADD COLUMN IF NOT EXISTS cat_id int REFERENCES todo_cats(id);
UPDATE todos SET cat_id = (SELECT id FROM todo_cats ORDER BY position, id LIMIT 1) WHERE cat_id IS NULL;

CREATE TABLE IF NOT EXISTS debts (
    id         serial PRIMARY KEY,
    direction  text NOT NULL CHECK (direction IN ('lent','borrowed')),
    name       text NOT NULL,
    amount     bigint NOT NULL,
    cash       boolean NOT NULL DEFAULT true,
    created_on date NOT NULL,
    settled_on date
);

CREATE TABLE IF NOT EXISTS sessions (
    token      text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash" {
		fmt.Fprintln(os.Stderr, "Escribe la contraseña y presiona Enter (se verá en pantalla):")
		r := bufio.NewReader(os.Stdin)
		pw, _ := r.ReadString('\n')
		pw = strings.TrimRight(pw, "\r\n")
		if len(pw) < 10 {
			log.Fatal("usa una contraseña de al menos 10 caracteres")
		}
		fmt.Println(hashPassword(pw))
		return
	}

	dsn := envOr("OINK_DSN", "")
	if dsn == "" {
		log.Fatal("falta OINK_DSN (ej. postgres://oink_user:pass@localhost/oink?sslmode=disable)")
	}
	hash := envOr("OINK_PASSWORD_HASH", "")
	if hash == "" {
		log.Fatal("falta OINK_PASSWORD_HASH (genera uno con: ./oink hash)")
	}
	addr := envOr("OINK_ADDR", "127.0.0.1:4100")
	loc, err := time.LoadLocation(envOr("OINK_TZ", "America/Mexico_City"))
	if err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(5)
	if err := db.Ping(); err != nil {
		log.Fatal("no pude conectar a Postgres: ", err)
	}
	if _, err := db.Exec(schema); err != nil {
		log.Fatal("migración: ", err)
	}

	app := &App{db: db, loc: loc, hash: hash, ver: assetVersion()}
	app.parseTemplates()

	mux := http.NewServeMux()

	// estáticos (embebidos en el binario); las URLs llevan ?v=<hash> así que
	// pueden cachearse fuerte: un deploy nuevo cambia la URL, no el caché
	static, _ := fsSub(staticFS, "static")
	mux.Handle("GET /static/", cacheControl("public, max-age=31536000, immutable",
		http.StripPrefix("/static/", http.FileServer(static))))
	mux.Handle("GET /manifest.webmanifest", serveStatic(static, "manifest.webmanifest", "application/manifest+json", "public, max-age=3600"))
	mux.Handle("GET /sw.js", serveStatic(static, "sw.js", "text/javascript", "no-cache"))
	mux.Handle("GET /favicon.svg", serveStatic(static, "favicon.svg", "image/svg+xml", "public, max-age=86400"))

	// sesión
	mux.HandleFunc("GET /login", app.loginPage)
	mux.HandleFunc("POST /login", app.loginPost)
	mux.HandleFunc("POST /logout", app.requireAuth(app.logout))

	// páginas
	mux.HandleFunc("GET /{$}", app.requireAuth(app.home))
	mux.HandleFunc("GET /entradas", app.requireAuth(app.incomesPage))
	mux.HandleFunc("GET /pendientes", app.requireAuth(app.todosPage))
	mux.HandleFunc("GET /reportes", app.requireAuth(app.reportsPage))
	mux.HandleFunc("GET /prestamos", app.requireAuth(app.debtsPage))
	mux.HandleFunc("GET /export.csv", app.requireAuth(app.exportCSV))
	// /mes se fusionó con /reportes y /fijos vive dentro de /ajustes
	mux.HandleFunc("GET /mes", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/reportes", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /fijos", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ajustes", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /ajustes", app.requireAuth(app.settingsPage))
	mux.HandleFunc("GET /tx/{id}", app.requireAuth(app.txEditPage))

	// acciones
	mux.HandleFunc("POST /tx", app.requireAuth(app.txCreate))
	mux.HandleFunc("POST /tx/{id}", app.requireAuth(app.txUpdate))
	mux.HandleFunc("POST /tx/{id}/delete", app.requireAuth(app.txDelete))
	mux.HandleFunc("POST /income/receive", app.requireAuth(app.incomeReceive))
	mux.HandleFunc("POST /income/add", app.requireAuth(app.incomeAdd))
	mux.HandleFunc("POST /fixed/pay", app.requireAuth(app.fixedPay))
	mux.HandleFunc("POST /template", app.requireAuth(app.templateCreate))
	mux.HandleFunc("POST /template/{id}", app.requireAuth(app.templateUpdate))
	mux.HandleFunc("POST /template/{id}/delete", app.requireAuth(app.templateDelete))
	mux.HandleFunc("POST /ajustes", app.requireAuth(app.settingsPost))
	mux.HandleFunc("POST /cycle/close", app.requireAuth(app.cycleClose))
	mux.HandleFunc("POST /debt", app.requireAuth(app.debtCreate))
	mux.HandleFunc("POST /debt/{id}/settle", app.requireAuth(app.debtSettle))
	mux.HandleFunc("POST /debt/{id}/delete", app.requireAuth(app.debtDelete))
	mux.HandleFunc("POST /todo", app.requireAuth(app.todoCreate))
	mux.HandleFunc("POST /todo/{id}/toggle", app.requireAuth(app.todoToggle))
	mux.HandleFunc("POST /todo/{id}/delete", app.requireAuth(app.todoDelete))
	mux.HandleFunc("POST /todocat", app.requireAuth(app.todoCatCreate))
	mux.HandleFunc("POST /todocat/{id}", app.requireAuth(app.todoCatUpdate))
	mux.HandleFunc("POST /todocat/{id}/delete", app.requireAuth(app.todoCatDelete))

	srv := &http.Server{
		Addr:         addr,
		Handler:      app.secure(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Println("oink escuchando en", addr)
	log.Fatal(srv.ListenAndServe())
}

// secure agrega headers de seguridad y verificación de Origin en POST.
func (a *App) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
		if r.Method == http.MethodPost {
			// defensa CSRF: la cookie ya es SameSite=Strict; además el Origin debe coincidir
			origin := r.Header.Get("Origin")
			if origin != "" && !sameHost(origin, r.Host) {
				http.Error(w, "origen no permitido", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	origin = strings.TrimPrefix(origin, "https://")
	origin = strings.TrimPrefix(origin, "http://")
	return strings.EqualFold(origin, host)
}

// assetVersion deriva un hash corto del css/js embebido: cambia con cada
// deploy que los toque y sirve para versionar sus URLs (?v=...).
func assetVersion() string {
	h := sha256.New()
	for _, p := range []string{"static/style.css", "static/app.js"} {
		b, _ := staticFS.ReadFile(p)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (a *App) today() time.Time {
	n := time.Now().In(a.loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, a.loc)
}
