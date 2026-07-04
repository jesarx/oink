package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Settings struct {
	SavingsGoal int64 // centavos
	WeeklyCash  int64
}

type Template struct {
	ID       int
	Kind     string
	Name     string
	Amount   int64
	Active   bool
	// estado dentro del ciclo abierto:
	DoneOn     sql.NullTime
	DoneAmount sql.NullInt64
}

type Cycle struct {
	ID        int
	StartedOn time.Time
	ClosedOn  sql.NullTime
}

type Tx struct {
	ID      int
	Kind    string
	Amount  int64
	Concept string
	Credit  bool
	MadeOn  time.Time
	TplName sql.NullString
}

type Dashboard struct {
	Cycle        Cycle
	Incomes      int64
	FixedPaid    int64
	FixedPending int64
	Variable     int64 // tarjeta + retiros
	CardTotal    int64
	CreditTotal  int64
	Available    int64
	Daily        int64
	DaysLeft     int
	DaysElapsed  int
	Envelope     int64 // lo que queda del sobre esta semana
	EnvelopeIn   int64 // retirado esta semana
	Status       string // ok | warn | over
	PctSpent     int    // % del presupuesto variable consumido (0-100 tope)
	PendingFixed int
}

func (a *App) getSettings() (Settings, error) {
	var s Settings
	err := a.db.QueryRow(`SELECT savings_goal, weekly_cash FROM settings WHERE id = 1`).
		Scan(&s.SavingsGoal, &s.WeeklyCash)
	return s, err
}

// openCycle regresa el ciclo abierto, creándolo si no existe ninguno.
func (a *App) openCycle() (Cycle, error) {
	var c Cycle
	err := a.db.QueryRow(`SELECT id, started_on, closed_on FROM cycles WHERE closed_on IS NULL ORDER BY started_on DESC LIMIT 1`).
		Scan(&c.ID, &c.StartedOn, &c.ClosedOn)
	if err == sql.ErrNoRows {
		today := a.today()
		err = a.db.QueryRow(`INSERT INTO cycles (started_on) VALUES ($1) RETURNING id, started_on`, today).
			Scan(&c.ID, &c.StartedOn)
	}
	return c, err
}

// rolloverCycle cierra el ciclo abierto (ayer) y abre uno nuevo hoy.
func (a *App) rolloverCycle() (Cycle, error) {
	today := a.today()
	cur, err := a.openCycle()
	if err != nil {
		return Cycle{}, err
	}
	closedOn := today.AddDate(0, 0, -1)
	if closedOn.Before(cur.StartedOn) {
		closedOn = cur.StartedOn
	}
	if _, err := a.db.Exec(`UPDATE cycles SET closed_on = $1 WHERE id = $2`, closedOn, cur.ID); err != nil {
		return Cycle{}, err
	}
	var c Cycle
	err = a.db.QueryRow(`INSERT INTO cycles (started_on) VALUES ($1) RETURNING id, started_on`, today).
		Scan(&c.ID, &c.StartedOn)
	return c, err
}

func (a *App) sumTx(cycleID int, where string, args ...any) int64 {
	var v sql.NullInt64
	q := `SELECT COALESCE(SUM(amount),0) FROM transactions WHERE cycle_id = $1 AND ` + where
	all := append([]any{cycleID}, args...)
	a.db.QueryRow(q, all...).Scan(&v)
	return v.Int64
}

func (a *App) loadDashboard() (Dashboard, Settings, error) {
	var d Dashboard
	s, err := a.getSettings()
	if err != nil {
		return d, s, err
	}
	c, err := a.openCycle()
	if err != nil {
		return d, s, err
	}
	d.Cycle = c
	d.Incomes = a.sumTx(c.ID, `kind = 'income'`)
	d.FixedPaid = a.sumTx(c.ID, `kind = 'fixed'`)
	d.CardTotal = a.sumTx(c.ID, `kind = 'card'`)
	d.CreditTotal = a.sumTx(c.ID, `kind = 'card' AND credit`)
	withdrawals := a.sumTx(c.ID, `kind = 'withdrawal'`)
	d.Variable = d.CardTotal + withdrawals

	// fijos pendientes: plantillas activas sin pago registrado en este ciclo
	var pending sql.NullInt64
	a.db.QueryRow(`
		SELECT COALESCE(SUM(t.amount),0), COUNT(*)
		FROM templates t
		WHERE t.kind = 'fixed' AND t.active
		  AND NOT EXISTS (SELECT 1 FROM transactions x
		                  WHERE x.cycle_id = $1 AND x.kind = 'fixed' AND x.template_id = t.id)`,
		c.ID).Scan(&pending, &d.PendingFixed)
	d.FixedPending = pending.Int64

	d.Available = d.Incomes - s.SavingsGoal - d.FixedPaid - d.FixedPending - d.Variable

	today := a.today()
	d.DaysElapsed = int(today.Sub(c.StartedOn).Hours()/24) + 1
	end := c.StartedOn.AddDate(0, 1, 0) // fin esperado: un mes después del inicio
	d.DaysLeft = int(end.Sub(today).Hours() / 24)
	if d.DaysLeft < 1 {
		d.DaysLeft = 1
	}
	if d.Available > 0 {
		d.Daily = d.Available / int64(d.DaysLeft)
	}

	// ritmo: fracción de presupuesto variable gastada vs fracción de tiempo transcurrida
	budget := d.Incomes - s.SavingsGoal - d.FixedPaid - d.FixedPending
	totalDays := d.DaysElapsed + d.DaysLeft - 1
	if totalDays < 1 {
		totalDays = 1
	}
	pctTime := float64(d.DaysElapsed) / float64(totalDays)
	var pctSpent float64
	if budget > 0 {
		pctSpent = float64(d.Variable) / float64(budget)
	} else if d.Variable > 0 {
		pctSpent = 1
	}
	d.PctSpent = int(pctSpent * 100)
	if d.PctSpent > 100 {
		d.PctSpent = 100
	}
	switch {
	case d.Available <= 0 || pctSpent >= 1:
		d.Status = "over"
	case pctSpent > pctTime+0.10:
		d.Status = "warn"
	default:
		d.Status = "ok"
	}

	// sobre semanal: semana lunes-domingo
	weekStart := today.AddDate(0, 0, -int((today.Weekday()+6)%7))
	d.EnvelopeIn = a.sumTx(c.ID, `kind = 'withdrawal' AND made_on >= $2`, weekStart)
	cashSpent := a.sumTx(c.ID, `kind = 'cash' AND made_on >= $2`, weekStart)
	d.Envelope = d.EnvelopeIn - cashSpent

	return d, s, nil
}

func (a *App) listTemplates(kind string, cycleID int) ([]Template, error) {
	rows, err := a.db.Query(`
		SELECT t.id, t.kind, t.name, t.amount, t.active, x.made_on, x.amount
		FROM templates t
		LEFT JOIN LATERAL (
			SELECT made_on, amount FROM transactions
			WHERE cycle_id = $2 AND template_id = t.id AND kind = t.kind
			ORDER BY made_on DESC LIMIT 1
		) x ON true
		WHERE t.kind = $1 AND t.active
		ORDER BY t.position, t.id`, kind, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Kind, &t.Name, &t.Amount, &t.Active, &t.DoneOn, &t.DoneAmount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *App) listTx(cycleID int) ([]Tx, error) {
	rows, err := a.db.Query(`
		SELECT x.id, x.kind, x.amount, x.concept, x.credit, x.made_on, t.name
		FROM transactions x LEFT JOIN templates t ON t.id = x.template_id
		WHERE x.cycle_id = $1
		ORDER BY x.made_on DESC, x.id DESC`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tx
	for rows.Next() {
		var t Tx
		if err := rows.Scan(&t.ID, &t.Kind, &t.Amount, &t.Concept, &t.Credit, &t.MadeOn, &t.TplName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *App) recentConcepts(kind string) []string {
	rows, err := a.db.Query(`
		SELECT concept FROM transactions
		WHERE kind = $1 AND concept <> '' AND created_at > now() - interval '90 days'
		GROUP BY concept ORDER BY COUNT(*) DESC, MAX(created_at) DESC LIMIT 8`, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// ---- formato ----

var spanishMonths = [...]string{"", "enero", "febrero", "marzo", "abril", "mayo", "junio",
	"julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

func fmtMoney(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100
	s := fmt.Sprintf("%d", whole)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	out := "$" + b.String()
	if frac != 0 {
		out += fmt.Sprintf(".%02d", frac)
	}
	if neg {
		out = "−" + out
	}
	return out
}

func fmtDate(t time.Time) string {
	return fmt.Sprintf("%d %s", t.Day(), spanishMonths[int(t.Month())][:3])
}

func fmtCycle(c Cycle) string {
	return fmt.Sprintf("desde el %d de %s", c.StartedOn.Day(), spanishMonths[int(c.StartedOn.Month())])
}

// parseMoney convierte "1234.56" o "1,234" a centavos.
func parseMoney(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(s, "$"), ",", ""))
	if s == "" {
		return 0, fmt.Errorf("monto vacío")
	}
	parts := strings.SplitN(s, ".", 2)
	var cents int64
	for _, ch := range parts[0] {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("monto inválido")
		}
		cents = cents*10 + int64(ch-'0')
		if cents > 1_000_000_000 {
			return 0, fmt.Errorf("monto demasiado grande")
		}
	}
	cents *= 100
	if len(parts) == 2 {
		f := parts[1]
		if len(f) > 2 {
			f = f[:2]
		}
		for len(f) < 2 {
			f += "0"
		}
		for _, ch := range f {
			if ch < '0' || ch > '9' {
				return 0, fmt.Errorf("monto inválido")
			}
		}
		cents += int64(f[0]-'0')*10 + int64(f[1]-'0')
	}
	return cents, nil
}
