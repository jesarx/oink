package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"strings"
	"time"
)

type Settings struct {
	SavingsGoal int64 // centavos
	WeeklyCash  int64
}

type Template struct {
	ID      int
	Kind    string
	Name    string
	Amount  int64
	Active  bool
	PayCash bool // fijos: se paga en efectivo, del sobre semanal
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
	ID       int
	Kind     string
	Amount   int64
	Concept  string
	Category string
	Credit   bool
	Cash     bool // entradas: llegó en efectivo (va al sobre semanal)
	MadeOn   time.Time
	TplName  sql.NullString
}

// rubros vigilados: gastos que el usuario quiere tener siempre a la vista.
var categories = []struct{ Key, Label string }{
	{"comida", "Comida"},
	{"libros", "Libros"},
	{"alcohol", "Alcohol"},
}

func validCategory(c string) bool {
	if c == "" {
		return true
	}
	for _, x := range categories {
		if x.Key == c {
			return true
		}
	}
	return false
}

type CategoryStat struct {
	Key    string
	Label  string
	Amount int64
	Pct    int // ancho de barra, relativo al rubro más grande
}

// categoryTotals suma lo gastado por rubro en el ciclo (para el home).
func (a *App) categoryTotals(cycleID int) []CategoryStat {
	out := make([]CategoryStat, 0, len(categories))
	for _, c := range categories {
		out = append(out, CategoryStat{Key: c.Key, Label: c.Label,
			Amount: a.sumTx(cycleID, `category = $2`, c.Key)})
	}
	return out
}

// CatDetail es un rubro con su total y la lista de gastos del periodo,
// para el desglose expandible de reportes.
type CatDetail struct {
	Stat CategoryStat
	Txs  []Tx
}

// categoryDetails arma total + movimientos de cada rubro vigilado dentro
// del periodo definido por where (columnas con prefijo x., ej. x.cycle_id = $1).
func (a *App) categoryDetails(where string, args ...any) []CatDetail {
	out := make([]CatDetail, 0, len(categories))
	var max int64
	n := len(args) + 1
	for _, c := range categories {
		txs, _ := a.listTxWhere(fmt.Sprintf("%s AND x.category = $%d", where, n),
			append(append([]any{}, args...), c.Key)...)
		var sum int64
		for _, t := range txs {
			sum += t.Amount
		}
		out = append(out, CatDetail{Stat: CategoryStat{Key: c.Key, Label: c.Label, Amount: sum}, Txs: txs})
		if sum > max {
			max = sum
		}
	}
	if max > 0 {
		for i := range out {
			out[i].Stat.Pct = int(out[i].Stat.Amount * 100 / max)
			if out[i].Stat.Pct < 4 && out[i].Stat.Amount > 0 {
				out[i].Stat.Pct = 4
			}
		}
	}
	return out
}

type Dashboard struct {
	Cycle        Cycle
	Incomes      int64
	FixedPaid    int64
	FixedPending int64
	Variable     int64 // tarjeta + retiros
	CardTotal    int64
	CreditTotal  int64
	DebitTotal   int64
	CreditDebt   int64 // crédito cargado y aún no pagado (todos los ciclos)
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
	return a.sumWhere(`cycle_id = $1 AND `+where, append([]any{cycleID}, args...)...)
}

// sumWhere suma montos sin acotar a un ciclo (p. ej. rangos de fechas).
func (a *App) sumWhere(where string, args ...any) int64 {
	var v sql.NullInt64
	a.db.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM transactions WHERE `+where, args...).Scan(&v)
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
	today := a.today()
	weekStart := today.AddDate(0, 0, -int((today.Weekday()+6)%7))

	// una sola consulta agregada para todo el tablero:
	// - los fijos pagados en efectivo salen del sobre (el retiro que los
	//   fondeó ya contó en el banco), solo cuentan los pagados del banco
	// - las entradas en efectivo van al sobre, no al banco
	// - los préstamos vía banco mueven el disponible; los de efectivo, el sobre
	var cashIn, withdrawals, loanOut, loanIn, weekOut int64
	err = a.db.QueryRow(`SELECT
		COALESCE(SUM(amount) FILTER (WHERE kind = 'income'), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'income' AND cash), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'fixed' AND NOT cash), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'card'), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'card' AND credit), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'withdrawal'), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'loan_out' AND NOT cash), 0),
		COALESCE(SUM(amount) FILTER (WHERE kind = 'loan_in' AND NOT cash), 0),
		COALESCE(SUM(amount) FILTER (WHERE made_on >= $2 AND (kind = 'withdrawal' OR (cash AND kind IN ('income','loan_in')))), 0),
		COALESCE(SUM(amount) FILTER (WHERE made_on >= $2 AND (kind = 'cash' OR (cash AND kind IN ('fixed','loan_out')))), 0)
		FROM transactions WHERE cycle_id = $1`, c.ID, weekStart).
		Scan(&d.Incomes, &cashIn, &d.FixedPaid, &d.CardTotal, &d.CreditTotal,
			&withdrawals, &loanOut, &loanIn, &d.EnvelopeIn, &weekOut)
	if err != nil {
		return d, s, err
	}
	d.DebitTotal = d.CardTotal - d.CreditTotal
	d.CreditDebt = a.creditDebt()
	d.Variable = d.CardTotal + withdrawals
	d.Envelope = d.EnvelopeIn - weekOut

	// fijos pendientes: plantillas activas sin pago registrado en este ciclo
	var pending sql.NullInt64
	a.db.QueryRow(`
		SELECT COALESCE(SUM(t.amount) FILTER (WHERE NOT t.pay_cash),0), COUNT(*)
		FROM templates t
		WHERE t.kind = 'fixed' AND t.active
		  AND NOT EXISTS (SELECT 1 FROM transactions x
		                  WHERE x.cycle_id = $1 AND x.kind = 'fixed' AND x.template_id = t.id)`,
		c.ID).Scan(&pending, &d.PendingFixed)
	d.FixedPending = pending.Int64

	d.Available = d.Incomes - cashIn - s.SavingsGoal - d.FixedPaid - d.FixedPending - d.Variable - loanOut + loanIn
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
	budget := d.Incomes - cashIn - s.SavingsGoal - d.FixedPaid - d.FixedPending - loanOut + loanIn
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

	return d, s, nil
}

func (a *App) listTemplates(kind string, cycleID int) ([]Template, error) {
	rows, err := a.db.Query(`
		SELECT t.id, t.kind, t.name, t.amount, t.active, t.pay_cash, x.made_on, x.amount
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
		if err := rows.Scan(&t.ID, &t.Kind, &t.Name, &t.Amount, &t.Active, &t.PayCash, &t.DoneOn, &t.DoneAmount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- pendientes (todos) ----

type Todo struct {
	ID        int
	Body      string
	DoneAt    sql.NullTime
	CreatedAt time.Time
}

// listTodos: abiertos en orden de llegada (el más viejo arriba);
// hechos del más reciente al más viejo, acotados.
func (a *App) listTodos(done bool) ([]Todo, error) {
	where, order, limit := "done_at IS NULL", "created_at ASC, id ASC", 200
	if done {
		where, order, limit = "done_at IS NOT NULL", "done_at DESC, id DESC", 30
	}
	rows, err := a.db.Query(fmt.Sprintf(`SELECT id, body, done_at, created_at
		FROM todos WHERE %s ORDER BY %s LIMIT %d`, where, order, limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Body, &t.DoneAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- préstamos (deudas con personas) ----

type Debt struct {
	ID        int
	Direction string // lent: presté | borrowed: me prestaron
	Name      string
	Amount    int64
	Cash      bool // efectivo (sobre) o tarjeta/banco
	CreatedOn time.Time
	SettledOn sql.NullTime
}

func (a *App) listDebts(direction string, settled bool) ([]Debt, error) {
	where, order, limit := "settled_on IS NULL", "created_on DESC, id DESC", 200
	if settled {
		where, order, limit = "settled_on IS NOT NULL", "settled_on DESC, id DESC", 15
	}
	rows, err := a.db.Query(fmt.Sprintf(`SELECT id, direction, name, amount, cash, created_on, settled_on
		FROM debts WHERE direction = $1 AND %s ORDER BY %s LIMIT %d`, where, order, limit), direction)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Debt
	for rows.Next() {
		var d Debt
		if err := rows.Scan(&d.ID, &d.Direction, &d.Name, &d.Amount, &d.Cash, &d.CreatedOn, &d.SettledOn); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// debtTotal suma lo pendiente por dirección (lent = me deben, borrowed = debo).
func (a *App) debtTotal(direction string) int64 {
	var v sql.NullInt64
	a.db.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM debts WHERE direction = $1 AND settled_on IS NULL`, direction).Scan(&v)
	return v.Int64
}

// creditDebt calcula la deuda viva de la tarjeta de crédito: todo lo cargado
// a crédito menos todos los pagos de tarjeta, a través de todos los ciclos.
func (a *App) creditDebt() int64 {
	var v sql.NullInt64
	a.db.QueryRow(`SELECT COALESCE(SUM(CASE
			WHEN kind = 'card' AND credit THEN amount
			WHEN kind = 'cardpay' THEN -amount
			ELSE 0 END), 0) FROM transactions`).Scan(&v)
	return v.Int64
}

// categorySeries regresa, por rubro, los totales alineados con cycleIDs
// (para las gráficas de avance mensual).
func (a *App) categorySeries(cycleIDs []int) map[string][]int64 {
	idx := make(map[int]int, len(cycleIDs))
	for i, id := range cycleIDs {
		idx[id] = i
	}
	out := make(map[string][]int64, len(categories))
	for _, c := range categories {
		out[c.Key] = make([]int64, len(cycleIDs))
	}
	rows, err := a.db.Query(`SELECT cycle_id, category, SUM(amount)
		FROM transactions WHERE category <> '' GROUP BY cycle_id, category`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var cat string
		var amt int64
		if rows.Scan(&cid, &cat, &amt) == nil {
			if i, ok := idx[cid]; ok {
				if s, ok := out[cat]; ok {
					s[i] = amt
				}
			}
		}
	}
	return out
}

// listExtraIncomes regresa las entradas no fijas (sin plantilla) del ciclo.
func (a *App) listExtraIncomes(cycleID int) ([]Tx, error) {
	rows, err := a.db.Query(`
		SELECT id, amount, concept, cash, made_on
		FROM transactions
		WHERE cycle_id = $1 AND kind = 'income' AND template_id IS NULL
		ORDER BY made_on DESC, id DESC`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tx
	for rows.Next() {
		t := Tx{Kind: "income"}
		if err := rows.Scan(&t.ID, &t.Amount, &t.Concept, &t.Cash, &t.MadeOn); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *App) listTx(cycleID int) ([]Tx, error) {
	return a.listTxWhere(`x.cycle_id = $1`, cycleID)
}

func (a *App) listTxWhere(where string, args ...any) ([]Tx, error) {
	rows, err := a.db.Query(`
		SELECT x.id, x.kind, x.amount, x.concept, x.category, x.credit, x.cash, x.made_on, t.name
		FROM transactions x LEFT JOIN templates t ON t.id = x.template_id
		WHERE `+where+`
		ORDER BY x.made_on DESC, x.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tx
	for rows.Next() {
		var t Tx
		if err := rows.Scan(&t.ID, &t.Kind, &t.Amount, &t.Concept, &t.Category, &t.Credit, &t.Cash, &t.MadeOn, &t.TplName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- reportes ----

type ConceptStat struct {
	Label  string
	Amount int64
	Count  int
	Pct    int // ancho de barra, relativo al concepto más grande
}

// conceptBreakdown agrupa por concepto lo realmente comprado en el ciclo
// (tarjeta, efectivo y fijos), ordenado de mayor a menor.
func (a *App) conceptBreakdown(cycleID int) ([]ConceptStat, int64) {
	rows, err := a.db.Query(`
		SELECT COALESCE(NULLIF(concept, ''), 'Sin concepto') AS label,
		       SUM(amount) AS total, COUNT(*) AS n
		FROM transactions
		WHERE cycle_id = $1 AND kind IN ('card','cash','fixed')
		GROUP BY label
		ORDER BY total DESC, label`, cycleID)
	if err != nil {
		return nil, 0
	}
	defer rows.Close()
	var out []ConceptStat
	var total, max int64
	for rows.Next() {
		var s ConceptStat
		if rows.Scan(&s.Label, &s.Amount, &s.Count) == nil {
			out = append(out, s)
			total += s.Amount
			if s.Amount > max {
				max = s.Amount
			}
		}
	}
	if max > 0 {
		for i := range out {
			out[i].Pct = int(out[i].Amount * 100 / max)
			if out[i].Pct < 4 && out[i].Amount > 0 {
				out[i].Pct = 4 // barra mínima visible
			}
		}
	}
	return out, total
}

type CycleStat struct {
	ID         int
	StartedOn  time.Time
	ClosedOn   sql.NullTime
	Income     int64
	CashIncome int64 // entradas en efectivo (fueron al sobre, no al banco)
	Spent      int64 // fijos + tarjeta + retiros (salida real del banco)
	LoanNet    int64 // préstamos vía banco: entradas menos salidas
	Saved      int64
}

// cycleHistory regresa los últimos ciclos con sus totales, para comparar meses.
func (a *App) cycleHistory(limit int) ([]CycleStat, error) {
	rows, err := a.db.Query(`
		SELECT c.id, c.started_on, c.closed_on,
		       COALESCE(SUM(x.amount) FILTER (WHERE x.kind = 'income'), 0),
		       COALESCE(SUM(x.amount) FILTER (WHERE x.kind = 'income' AND x.cash), 0),
		       COALESCE(SUM(x.amount) FILTER (WHERE x.kind IN ('card','withdrawal')
		                                       OR (x.kind = 'fixed' AND NOT x.cash)), 0),
		       COALESCE(SUM(CASE WHEN x.kind = 'loan_in' AND NOT x.cash THEN x.amount
		                         WHEN x.kind = 'loan_out' AND NOT x.cash THEN -x.amount
		                         ELSE 0 END), 0)
		FROM cycles c
		LEFT JOIN transactions x ON x.cycle_id = c.id
		GROUP BY c.id
		ORDER BY c.started_on DESC, c.id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CycleStat
	for rows.Next() {
		var s CycleStat
		if err := rows.Scan(&s.ID, &s.StartedOn, &s.ClosedOn, &s.Income, &s.CashIncome, &s.Spent, &s.LoanNet); err != nil {
			return nil, err
		}
		s.Saved = s.Income - s.CashIncome - s.Spent + s.LoanNet
		out = append(out, s)
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

// fmtMonth: "julio 2026" a partir de una fecha (para encabezados de reportes).
func fmtMonth(t time.Time) string {
	return fmt.Sprintf("%s %d", spanishMonths[int(t.Month())], t.Year())
}

// compactMoney abrevia montos para etiquetas de gráfica: "$850", "$8.2k".
func compactMoney(cents int64) string {
	pesos := cents / 100
	switch {
	case pesos >= 10000:
		return fmt.Sprintf("$%.0fk", float64(pesos)/1000)
	case pesos >= 1000:
		return fmt.Sprintf("$%.1fk", float64(pesos)/1000)
	}
	return fmt.Sprintf("$%d", pesos)
}

// barSVG genera una gráfica de barras en SVG (sin JS ni libs externas,
// compatible con la CSP). El texto se estiliza desde style.css.
func barSVG(labels []string, vals []int64, color string) template.HTML {
	n := len(vals)
	if n == 0 {
		return ""
	}
	const bw, gap, padX, chartH, topPad, labelH = 34, 10, 6, 110, 16, 16
	w := padX*2 + n*bw + (n-1)*gap
	h := topPad + chartH + labelH
	var max int64 = 1
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg width="%d" height="%d" viewBox="0 0 %d %d" class="chart" role="img">`, w, h, w, h)
	fmt.Fprintf(&b, `<line x1="0" y1="%d" x2="%d" y2="%d" class="axis"/>`, topPad+chartH, w, topPad+chartH)
	for i, v := range vals {
		x := padX + i*(bw+gap)
		bh := int(int64(chartH) * v / max)
		if v > 0 && bh < 3 {
			bh = 3
		}
		y := topPad + chartH - bh
		if v > 0 {
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="4" fill="%s"/>`, x, y, bw, bh, color)
			fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" class="cval">%s</text>`, x+bw/2, y-4, compactMoney(v))
		}
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" class="clab">%s</text>`,
			x+bw/2, topPad+chartH+12, template.HTMLEscapeString(labels[i]))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
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
