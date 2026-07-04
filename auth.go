package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "oink_session"
	sessionTTL    = 30 * 24 * time.Hour
	kdfIter       = 300_000
	kdfKeyLen     = 32
)

// ---- PBKDF2-HMAC-SHA256 (RFC 8018), implementado sobre la stdlib ----

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, numBlocks*hLen)
	var block [4]byte
	U := make([]byte, hLen)
	T := make([]byte, hLen)
	for b := 1; b <= numBlocks; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(block[:], uint32(b))
		prf.Write(block[:])
		U = prf.Sum(U[:0])
		copy(T, U)
		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(U)
			U = prf.Sum(U[:0])
			for j := range T {
				T[j] ^= U[j]
			}
		}
		dk = append(dk, T...)
	}
	return dk[:keyLen]
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := pbkdf2SHA256([]byte(pw), salt, kdfIter, kdfKeyLen)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", kdfIter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- rate limit de login (en memoria; un solo usuario) ----

var loginGuard = struct {
	sync.Mutex
	fails int
	until time.Time
}{}

func loginLocked() (bool, time.Duration) {
	loginGuard.Lock()
	defer loginGuard.Unlock()
	if time.Now().Before(loginGuard.until) {
		return true, time.Until(loginGuard.until)
	}
	return false, 0
}

func loginFailed() {
	loginGuard.Lock()
	defer loginGuard.Unlock()
	loginGuard.fails++
	if loginGuard.fails >= 5 {
		loginGuard.until = time.Now().Add(15 * time.Minute)
		loginGuard.fails = 0
	}
}

func loginOK() {
	loginGuard.Lock()
	defer loginGuard.Unlock()
	loginGuard.fails = 0
	loginGuard.until = time.Time{}
}

// ---- sesiones ----

func (a *App) createSession(w http.ResponseWriter) error {
	raw := make([]byte, 32)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	exp := time.Now().Add(sessionTTL)
	if _, err := a.db.Exec(`INSERT INTO sessions (token, expires_at) VALUES ($1, $2)`, token, exp); err != nil {
		return err
	}
	a.db.Exec(`DELETE FROM sessions WHERE expires_at < now()`)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		Expires: exp, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (a *App) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 64 {
		return false
	}
	var ok bool
	err = a.db.QueryRow(`SELECT true FROM sessions WHERE token = $1 AND expires_at > now()`, c.Value).Scan(&ok)
	return err == nil && ok
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.validSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.validSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "login.html", map[string]any{"Error": ""})
}

func (a *App) loginPost(w http.ResponseWriter, r *http.Request) {
	if locked, wait := loginLocked(); locked {
		a.render(w, "login.html", map[string]any{
			"Error": fmt.Sprintf("Demasiados intentos. Espera %d minutos.", int(wait.Minutes())+1)})
		return
	}
	pw := r.FormValue("password")
	if !verifyPassword(pw, a.hash) {
		loginFailed()
		time.Sleep(600 * time.Millisecond)
		a.render(w, "login.html", map[string]any{"Error": "Contraseña incorrecta."})
		return
	}
	loginOK()
	if err := a.createSession(w); err != nil {
		http.Error(w, "error de sesión", 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE token = $1`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
