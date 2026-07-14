package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (a *App) parseTemplates() {
	funcs := template.FuncMap{
		"money": fmtMoney,
		"date":  fmtDate,
		"cycle": fmtCycle,
		"month": fmtMonth,
		"v":     func() string { return a.ver },
		"amt": func(cents int64) string {
			if cents%100 == 0 {
				return strconv.FormatInt(cents/100, 10)
			}
			return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
		},
		"pct": func(part, whole int64) int {
			if whole <= 0 {
				return 0
			}
			return int(part * 100 / whole)
		},
		"catLabel": func(key string) string {
			for _, c := range categories {
				if c.Key == key {
					return c.Label
				}
			}
			return key
		},
		"kindLabel": func(k string, credit bool) string {
			switch k {
			case "card":
				if credit {
					return "T. crédito"
				}
				return "T. débito"
			case "cash":
				return "Efectivo"
			case "cardpay":
				return "Pago t. crédito"
			case "loan_out":
				return "Préstamo · salida"
			case "loan_in":
				return "Préstamo · entrada"
			case "withdrawal":
				return "Retiro de cajero"
			case "income":
				return "Entrada"
			case "fixed":
				return "Gasto fijo"
			}
			return k
		},
	}
	pages := []string{"login.html", "home.html", "incomes.html", "pendientes.html", "reportes.html", "prestamos.html", "settings.html", "txedit.html"}
	a.tmpl = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t := template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+p))
		a.tmpl[p] = t
	}
}

func (a *App) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl[page].ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Println("render:", err)
	}
}

func (a *App) fail(w http.ResponseWriter, err error) {
	log.Println("error:", err)
	http.Error(w, "algo salió mal: "+err.Error(), http.StatusInternalServerError)
}

// ---- home ----

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	d, s, err := a.loadDashboard()
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, "home.html", map[string]any{
		"Nav": "home", "D": d, "S": s,
		"Cats":         a.categoryTotals(d.Cycle.ID),
		"CardConcepts": a.recentConcepts("card"),
		"CashConcepts": a.recentConcepts("cash"),
	})
}

// ---- transacciones ----

func (a *App) txCreate(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	if kind != "card" && kind != "cash" && kind != "withdrawal" && kind != "cardpay" {
		http.Error(w, "tipo inválido", 400)
		return
	}
	amount, err := parseMoney(r.FormValue("amount"))
	if err != nil || amount <= 0 {
		http.Error(w, "monto inválido", 400)
		return
	}
	category := r.FormValue("category")
	if !validCategory(category) {
		http.Error(w, "rubro inválido", 400)
		return
	}
	if kind == "withdrawal" || kind == "cardpay" {
		category = "" // ni retiros ni pagos de tarjeta son gastos de rubro
	}
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	concept := strings.TrimSpace(r.FormValue("concept"))
	credit := kind == "card" && r.FormValue("credit") != "off"

	// captura offline: el cliente manda la fecha real del gasto (por si se
	// sincroniza días después) y un client_id único; un reintento con el
	// mismo client_id no duplica (ON CONFLICT DO NOTHING).
	madeOn := a.today()
	if v := r.FormValue("made_on"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, a.loc); err == nil &&
			!t.After(madeOn) && t.After(madeOn.AddDate(0, -2, 0)) {
			madeOn = t
		}
	}
	clientID := sql.NullString{}
	if v := strings.TrimSpace(r.FormValue("client_id")); v != "" && len(v) <= 64 {
		clientID = sql.NullString{String: v, Valid: true}
	}
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, amount, concept, category, credit, made_on, client_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (client_id) DO NOTHING`, c.ID, kind, amount, concept, category, credit, madeOn, clientID)
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) txEditPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var t Tx
	err := a.db.QueryRow(`SELECT x.id, x.kind, x.amount, x.concept, x.category, x.credit, x.cash, x.made_on, t.name
		FROM transactions x LEFT JOIN templates t ON t.id = x.template_id WHERE x.id = $1`, id).
		Scan(&t.ID, &t.Kind, &t.Amount, &t.Concept, &t.Category, &t.Credit, &t.Cash, &t.MadeOn, &t.TplName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, "txedit.html", map[string]any{"Nav": "reportes", "T": t, "Categories": categories,
		"Amount": strconv.FormatFloat(float64(t.Amount)/100, 'f', -1, 64),
		"Date":   t.MadeOn.Format("2006-01-02")})
}

func (a *App) txUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	amount, err := parseMoney(r.FormValue("amount"))
	if err != nil || amount <= 0 {
		http.Error(w, "monto inválido", 400)
		return
	}
	madeOn, err := time.ParseInLocation("2006-01-02", r.FormValue("made_on"), a.loc)
	if err != nil {
		http.Error(w, "fecha inválida", 400)
		return
	}
	category := r.FormValue("category")
	if !validCategory(category) {
		http.Error(w, "rubro inválido", 400)
		return
	}
	concept := strings.TrimSpace(r.FormValue("concept"))

	var kind string
	var credit, cash bool
	if err := a.db.QueryRow(`SELECT kind, credit, cash FROM transactions WHERE id=$1`, id).
		Scan(&kind, &credit, &cash); err != nil {
		http.NotFound(w, r)
		return
	}
	if kind == "income" || kind == "fixed" {
		// entradas y fijos pueden corregirse entre banco y efectivo (sobre)
		switch r.FormValue("via") {
		case "banco":
			cash = false
		case "efectivo":
			cash = true
		case "":
			// sin cambio
		default:
			http.Error(w, "fuente inválida", 400)
			return
		}
		_, err = a.db.Exec(`UPDATE transactions SET amount=$1, concept=$2, made_on=$3, cash=$4 WHERE id=$5`,
			amount, concept, madeOn, cash, id)
	} else if kind == "card" || kind == "cash" {
		// la fuente puede cambiarse entre t. débito, t. crédito y efectivo
		switch r.FormValue("source") {
		case "debit":
			kind, credit = "card", false
		case "credit":
			kind, credit = "card", true
		case "cash":
			kind, credit = "cash", false
		case "":
			// sin cambio
		default:
			http.Error(w, "fuente inválida", 400)
			return
		}
		_, err = a.db.Exec(`UPDATE transactions SET amount=$1, concept=$2, made_on=$3,
			kind=$4, credit=$5, category=$6 WHERE id=$7`,
			amount, concept, madeOn, kind, credit, category, id)
	} else {
		_, err = a.db.Exec(`UPDATE transactions SET amount=$1, concept=$2, made_on=$3 WHERE id=$4`,
			amount, concept, madeOn, id)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/reportes", http.StatusSeeOther)
}

func (a *App) txDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	a.db.Exec(`DELETE FROM transactions WHERE id = $1`, id)
	http.Redirect(w, r, "/reportes", http.StatusSeeOther)
}

// ---- entradas ----

func (a *App) incomesPage(w http.ResponseWriter, r *http.Request) {
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	tpls, err := a.listTemplates("income", c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	extras, err := a.listExtraIncomes(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, "incomes.html", map[string]any{"Nav": "entradas", "Templates": tpls, "Extras": extras, "Cycle": c})
}

// incomeAdd registra una entrada no fija (única) en el ciclo actual, sin
// disparar el cierre de mes que sí provocan las entradas fijas.
func (a *App) incomeAdd(w http.ResponseWriter, r *http.Request) {
	concept := strings.TrimSpace(r.FormValue("concept"))
	amount, err := parseMoney(r.FormValue("amount"))
	if concept == "" || err != nil || amount <= 0 {
		http.Error(w, "datos inválidos", 400)
		return
	}
	cash := r.FormValue("via") == "efectivo"
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, amount, concept, cash, made_on)
		VALUES ($1,'income',$2,$3,$4,$5)`, c.ID, amount, concept, cash, a.today())
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/entradas", http.StatusSeeOther)
}

func (a *App) incomeReceive(w http.ResponseWriter, r *http.Request) {
	tplID, _ := strconv.Atoi(r.FormValue("template_id"))
	var tpl Template
	err := a.db.QueryRow(`SELECT id, name, amount FROM templates WHERE id = $1 AND kind = 'income' AND active`, tplID).
		Scan(&tpl.ID, &tpl.Name, &tpl.Amount)
	if err != nil {
		http.Error(w, "entrada no encontrada", 404)
		return
	}
	amount := tpl.Amount
	if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
		if amount, err = parseMoney(v); err != nil || amount <= 0 {
			http.Error(w, "monto inválido", 400)
			return
		}
	}
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	var already bool
	a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM transactions
		WHERE cycle_id=$1 AND kind='income' AND template_id=$2)`, c.ID, tpl.ID).Scan(&already)
	if already {
		if r.FormValue("rollover") != "1" {
			http.Error(w, "esta entrada ya se registró en el ciclo actual", 409)
			return
		}
		if c, err = a.rolloverCycle(); err != nil {
			a.fail(w, err)
			return
		}
	}
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, template_id, amount, concept, made_on)
		VALUES ($1,'income',$2,$3,$4,$5)`, c.ID, tpl.ID, amount, tpl.Name, a.today())
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/entradas", http.StatusSeeOther)
}

// ---- fijos (viven dentro de Ajustes) ----

func (a *App) fixedPay(w http.ResponseWriter, r *http.Request) {
	tplID, _ := strconv.Atoi(r.FormValue("template_id"))
	var tpl Template
	err := a.db.QueryRow(`SELECT id, name, amount, pay_cash FROM templates WHERE id = $1 AND kind = 'fixed' AND active`, tplID).
		Scan(&tpl.ID, &tpl.Name, &tpl.Amount, &tpl.PayCash)
	if err != nil {
		http.Error(w, "gasto fijo no encontrado", 404)
		return
	}
	amount := tpl.Amount
	if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
		if amount, err = parseMoney(v); err != nil || amount <= 0 {
			http.Error(w, "monto inválido", 400)
			return
		}
	}
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	var already bool
	a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM transactions
		WHERE cycle_id=$1 AND kind='fixed' AND template_id=$2)`, c.ID, tpl.ID).Scan(&already)
	if already {
		http.Error(w, "este fijo ya se pagó en el ciclo actual", 409)
		return
	}
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, template_id, amount, concept, cash, made_on)
		VALUES ($1,'fixed',$2,$3,$4,$5,$6)`, c.ID, tpl.ID, amount, tpl.Name, tpl.PayCash, a.today())
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ajustes", http.StatusSeeOther)
}

// ---- plantillas (CRUD) ----

func (a *App) templateCreate(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	if kind != "income" && kind != "fixed" {
		http.Error(w, "tipo inválido", 400)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	amount, err := parseMoney(r.FormValue("amount"))
	if name == "" || err != nil || amount <= 0 {
		http.Error(w, "datos inválidos", 400)
		return
	}
	if kind == "income" {
		var n int
		a.db.QueryRow(`SELECT COUNT(*) FROM templates WHERE kind='income' AND active`).Scan(&n)
		if n >= 5 {
			http.Error(w, "máximo 5 entradas fijas", 400)
			return
		}
	}
	payCash := kind == "fixed" && r.FormValue("pay") == "efectivo"
	a.db.Exec(`INSERT INTO templates (kind, name, amount, pay_cash) VALUES ($1,$2,$3,$4)`, kind, name, amount, payCash)
	a.redirectByKind(w, r, kind)
}

func (a *App) templateUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	amount, err := parseMoney(r.FormValue("amount"))
	if name == "" || err != nil || amount <= 0 {
		http.Error(w, "datos inválidos", 400)
		return
	}
	var kind string
	// pay solo viene en el formulario de fijos; vacío = conservar el actual
	if err := a.db.QueryRow(`UPDATE templates SET name=$1, amount=$2,
		pay_cash = CASE WHEN $3 = '' THEN pay_cash ELSE $3 = 'efectivo' END
		WHERE id=$4 RETURNING kind`,
		name, amount, r.FormValue("pay"), id).Scan(&kind); err != nil {
		http.NotFound(w, r)
		return
	}
	a.redirectByKind(w, r, kind)
}

func (a *App) templateDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var kind string
	if err := a.db.QueryRow(`UPDATE templates SET active = false WHERE id=$1 RETURNING kind`, id).Scan(&kind); err != nil {
		http.NotFound(w, r)
		return
	}
	a.redirectByKind(w, r, kind)
}

func (a *App) redirectByKind(w http.ResponseWriter, r *http.Request, kind string) {
	if kind == "income" {
		http.Redirect(w, r, "/entradas", http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/ajustes", http.StatusSeeOther)
	}
}

// ---- reportes ----

func (a *App) reportsPage(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("v") {
	case "semana":
		a.reportsWeek(w, r)
	case "graficas":
		a.reportsCharts(w, r)
	default:
		a.reportsMonth(w, r)
	}
}

func (a *App) reportsMonth(w http.ResponseWriter, r *http.Request) {
	cur, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	c := cur
	if v := r.URL.Query().Get("c"); v != "" {
		id, _ := strconv.Atoi(v)
		var cc Cycle
		if a.db.QueryRow(`SELECT id, started_on, closed_on FROM cycles WHERE id = $1`, id).
			Scan(&cc.ID, &cc.StartedOn, &cc.ClosedOn) == nil {
			c = cc
		}
	}
	var prev, next sql.NullInt64
	a.db.QueryRow(`SELECT id FROM cycles WHERE (started_on, id) < ($1, $2) ORDER BY started_on DESC, id DESC LIMIT 1`,
		c.StartedOn, c.ID).Scan(&prev)
	a.db.QueryRow(`SELECT id FROM cycles WHERE (started_on, id) > ($1, $2) ORDER BY started_on ASC, id ASC LIMIT 1`,
		c.StartedOn, c.ID).Scan(&next)

	incomes := a.sumTx(c.ID, `kind = 'income'`)
	cashIn := a.sumTx(c.ID, `kind = 'income' AND cash`)
	fixed := a.sumTx(c.ID, `kind = 'fixed' AND NOT cash`)
	fixedCash := a.sumTx(c.ID, `kind = 'fixed' AND cash`)
	card := a.sumTx(c.ID, `kind = 'card'`)
	creditCard := a.sumTx(c.ID, `kind = 'card' AND credit`)
	withdrawals := a.sumTx(c.ID, `kind = 'withdrawal'`)
	cardPay := a.sumTx(c.ID, `kind = 'cardpay'`)
	loanOut := a.sumTx(c.ID, `kind = 'loan_out' AND NOT cash`)
	loanIn := a.sumTx(c.ID, `kind = 'loan_in' AND NOT cash`)
	spent := fixed + card + withdrawals
	saved := incomes - cashIn - spent - loanOut + loanIn

	concepts, conceptTotal := a.conceptBreakdown(c.ID)
	history, err := a.cycleHistory(12)
	if err != nil {
		a.fail(w, err)
		return
	}
	txs, err := a.listTx(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}

	a.render(w, "reportes.html", map[string]any{
		"Nav": "reportes", "View": "mes", "C": c, "IsCurrent": c.ID == cur.ID,
		"Prev": prev, "Next": next,
		"Incomes": incomes, "CashIn": cashIn, "Fixed": fixed, "FixedCash": fixedCash,
		"Debit": card - creditCard,
		"CreditCard": creditCard, "Withdrawals": withdrawals, "CardPay": cardPay,
		"LoanOut": loanOut, "LoanIn": loanIn,
		"Spent": spent, "Saved": saved, "Txs": txs,
		"CatsD":    a.categoryDetails(`x.cycle_id = $1`, c.ID),
		"Concepts": concepts, "ConceptTotal": conceptTotal, "History": history,
	})
}

func (a *App) reportsWeek(w http.ResponseWriter, r *http.Request) {
	off, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if off > 0 {
		off = 0
	}
	today := a.today()
	ws := today.AddDate(0, 0, -int((today.Weekday()+6)%7)+7*off) // lunes
	we := ws.AddDate(0, 0, 7)

	rng := `made_on >= $1 AND made_on < $2`
	debit := a.sumWhere(`kind = 'card' AND NOT credit AND `+rng, ws, we)
	creditCard := a.sumWhere(`kind = 'card' AND credit AND `+rng, ws, we)
	cashSpent := a.sumWhere(`kind = 'cash' AND `+rng, ws, we)
	fixed := a.sumWhere(`kind = 'fixed' AND `+rng, ws, we)

	a.render(w, "reportes.html", map[string]any{
		"Nav": "reportes", "View": "semana",
		"WeekLabel": fmtDate(ws) + " – " + fmtDate(we.AddDate(0, 0, -1)),
		"IsCurrentWeek": off == 0, "PrevW": off - 1, "NextW": off + 1,
		"Debit": debit, "CreditCard": creditCard, "CashSpent": cashSpent, "Fixed": fixed,
		"WeekSpent": debit + creditCard + cashSpent + fixed,
		"CatsD":     a.categoryDetails(`x.made_on >= $1 AND x.made_on < $2`, ws, we),
	})
}

func (a *App) reportsCharts(w http.ResponseWriter, r *http.Request) {
	history, err := a.cycleHistory(12)
	if err != nil {
		a.fail(w, err)
		return
	}
	// series cronológicas (viejo -> nuevo)
	chron := make([]CycleStat, len(history))
	for i, h := range history {
		chron[len(history)-1-i] = h
	}
	labels := make([]string, len(chron))
	spentVals := make([]int64, len(chron))
	ids := make([]int, len(chron))
	for i, h := range chron {
		labels[i] = fmtDate(h.StartedOn)
		spentVals[i] = h.Spent
		ids[i] = h.ID
	}
	catSeries := a.categorySeries(ids)
	catColors := map[string]string{"comida": "#6fa8e8", "libros": "#55c99a", "alcohol": "#efa13b"}
	type catChart struct {
		Label string
		SVG   template.HTML
	}
	var catCharts []catChart
	for _, cc := range categories {
		catCharts = append(catCharts, catChart{cc.Label, barSVG(labels, catSeries[cc.Key], catColors[cc.Key])})
	}
	a.render(w, "reportes.html", map[string]any{
		"Nav": "reportes", "View": "graficas",
		"SpentChart": barSVG(labels, spentVals, "#e06a93"),
		"CatCharts":  catCharts,
	})
}

// ---- pendientes ----

func (a *App) todosPage(w http.ResponseWriter, r *http.Request) {
	cats, err := a.listTodoCats()
	if err != nil || len(cats) == 0 {
		a.fail(w, fmt.Errorf("categorías de pendientes: %v", err))
		return
	}
	cur := cats[0]
	if v, _ := strconv.Atoi(r.URL.Query().Get("c")); v > 0 {
		for _, c := range cats {
			if c.ID == v {
				cur = c
				break
			}
		}
	}
	open, err := a.listTodos(cur.ID, false)
	if err != nil {
		a.fail(w, err)
		return
	}
	done, _ := a.listTodos(cur.ID, true)
	a.render(w, "pendientes.html", map[string]any{
		"Nav": "pendientes", "Cats": cats, "Cur": cur, "Open": open, "Done": done})
}

func (a *App) todoCreate(w http.ResponseWriter, r *http.Request) {
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" || len(body) > 1000 {
		http.Error(w, "texto inválido", 400)
		return
	}
	// si la categoría no existe (p. ej. se borró mientras el registro
	// esperaba en la cola offline), cae a la primera
	catID, _ := strconv.Atoi(r.FormValue("cat_id"))
	var ok bool
	a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM todo_cats WHERE id = $1)`, catID).Scan(&ok)
	if !ok {
		if err := a.db.QueryRow(`SELECT id FROM todo_cats ORDER BY position, id LIMIT 1`).Scan(&catID); err != nil {
			a.fail(w, err)
			return
		}
	}
	clientID := sql.NullString{}
	if v := strings.TrimSpace(r.FormValue("client_id")); v != "" && len(v) <= 64 {
		clientID = sql.NullString{String: v, Valid: true}
	}
	// mismo esquema de idempotencia que los gastos: la cola offline puede
	// reintentar sin duplicar
	if _, err := a.db.Exec(`INSERT INTO todos (body, cat_id, client_id) VALUES ($1,$2,$3)
		ON CONFLICT (client_id) DO NOTHING`, body, catID, clientID); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pendientes?c=%d", catID), http.StatusSeeOther)
}

func (a *App) todoToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	// el cliente manda el estado deseado ("set") para que un reintento de la
	// cola offline sea idempotente: aplicar dos veces no revierte nada
	switch r.FormValue("set") {
	case "done":
		a.db.Exec(`UPDATE todos SET done_at = now() WHERE id = $1 AND done_at IS NULL`, id)
	case "open":
		a.db.Exec(`UPDATE todos SET done_at = NULL WHERE id = $1`, id)
	default:
		a.db.Exec(`UPDATE todos SET done_at = CASE WHEN done_at IS NULL THEN now() ELSE NULL END WHERE id = $1`, id)
	}
	http.Redirect(w, r, a.todoCatURL(id), http.StatusSeeOther)
}

func (a *App) todoDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	dest := a.todoCatURL(id)
	a.db.Exec(`DELETE FROM todos WHERE id = $1`, id)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// todoCatURL regresa la pestaña de la categoría del pendiente, para volver
// a donde estaba el usuario después de una acción.
func (a *App) todoCatURL(todoID int) string {
	var cat sql.NullInt64
	a.db.QueryRow(`SELECT cat_id FROM todos WHERE id = $1`, todoID).Scan(&cat)
	if cat.Valid {
		return fmt.Sprintf("/pendientes?c=%d", cat.Int64)
	}
	return "/pendientes"
}

// ---- categorías de pendientes (se administran en Ajustes) ----

func validCatName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= 12
}

func (a *App) todoCatCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if !validCatName(name) {
		http.Error(w, "nombre inválido (máximo 12 caracteres)", 400)
		return
	}
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM todo_cats`).Scan(&n)
	if n >= 3 {
		http.Error(w, "máximo 3 categorías", 400)
		return
	}
	a.db.Exec(`INSERT INTO todo_cats (name, position) VALUES ($1, $2)`, name, n)
	http.Redirect(w, r, "/ajustes", http.StatusSeeOther)
}

func (a *App) todoCatUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if !validCatName(name) {
		http.Error(w, "nombre inválido (máximo 12 caracteres)", 400)
		return
	}
	a.db.Exec(`UPDATE todo_cats SET name = $1 WHERE id = $2`, name, id)
	http.Redirect(w, r, "/ajustes", http.StatusSeeOther)
}

func (a *App) todoCatDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM todo_cats`).Scan(&n)
	if n <= 1 {
		http.Error(w, "debe existir al menos una categoría", 400)
		return
	}
	// sus pendientes se mudan a la primera categoría restante
	if _, err := a.db.Exec(`UPDATE todos SET cat_id =
		(SELECT id FROM todo_cats WHERE id <> $1 ORDER BY position, id LIMIT 1)
		WHERE cat_id = $1`, id); err != nil {
		a.fail(w, err)
		return
	}
	a.db.Exec(`DELETE FROM todo_cats WHERE id = $1`, id)
	http.Redirect(w, r, "/ajustes", http.StatusSeeOther)
}

// ---- préstamos ----

func (a *App) debtsPage(w http.ResponseWriter, r *http.Request) {
	lent, err := a.listDebts("lent", false)
	if err != nil {
		a.fail(w, err)
		return
	}
	borrowed, _ := a.listDebts("borrowed", false)
	lentDone, _ := a.listDebts("lent", true)
	borrowedDone, _ := a.listDebts("borrowed", true)
	a.render(w, "prestamos.html", map[string]any{
		"Nav": "prestamos",
		"Lent": lent, "Borrowed": borrowed,
		"LentDone": lentDone, "BorrowedDone": borrowedDone,
		"OwedToMe": a.debtTotal("lent"), "IOwe": a.debtTotal("borrowed"),
	})
}

func (a *App) debtCreate(w http.ResponseWriter, r *http.Request) {
	direction := r.FormValue("direction")
	if direction != "lent" && direction != "borrowed" {
		http.Error(w, "tipo inválido", 400)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	amount, err := parseMoney(r.FormValue("amount"))
	if name == "" || err != nil || amount <= 0 {
		http.Error(w, "datos inválidos", 400)
		return
	}
	cash := r.FormValue("source") != "card" // efectivo por default
	if _, err := a.db.Exec(`INSERT INTO debts (direction, name, amount, cash, created_on)
		VALUES ($1,$2,$3,$4,$5)`, direction, name, amount, cash, a.today()); err != nil {
		a.fail(w, err)
		return
	}
	// con el checkbox activo, el préstamo mueve el saldo de inmediato:
	// presté = salida (baja sobre o disponible), me prestaron = entrada
	if r.FormValue("apply") == "on" {
		kind, concept := "loan_out", "Presté a "+name
		if direction == "borrowed" {
			kind, concept = "loan_in", "Me prestó "+name
		}
		if err := a.insertLoanTx(kind, amount, concept, cash); err != nil {
			a.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/prestamos", http.StatusSeeOther)
}

func (a *App) debtSettle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var d Debt
	err := a.db.QueryRow(`UPDATE debts SET settled_on = $1 WHERE id = $2 AND settled_on IS NULL
		RETURNING direction, name, amount, cash`, a.today(), id).
		Scan(&d.Direction, &d.Name, &d.Amount, &d.Cash)
	if err != nil {
		http.Error(w, "préstamo no encontrado o ya saldado", 404)
		return
	}
	// saldar invierte el flujo original, si se pide reflejarlo en el saldo
	if r.FormValue("apply") == "on" {
		kind, concept := "loan_in", "Me pagó "+d.Name
		if d.Direction == "borrowed" {
			kind, concept = "loan_out", "Pagué a "+d.Name
		}
		if err := a.insertLoanTx(kind, d.Amount, concept, d.Cash); err != nil {
			a.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/prestamos", http.StatusSeeOther)
}

func (a *App) debtDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	a.db.Exec(`DELETE FROM debts WHERE id = $1`, id)
	http.Redirect(w, r, "/prestamos", http.StatusSeeOther)
}

func (a *App) insertLoanTx(kind string, amount int64, concept string, cash bool) error {
	c, err := a.openCycle()
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, amount, concept, cash, made_on)
		VALUES ($1,$2,$3,$4,$5,$6)`, c.ID, kind, amount, concept, cash, a.today())
	return err
}

// exportCSV descarga todos los movimientos de todos los ciclos.
func (a *App) exportCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`
		SELECT x.made_on, c.started_on, x.kind, x.credit, x.cash, x.category, x.concept, x.amount
		FROM transactions x JOIN cycles c ON c.id = x.cycle_id
		ORDER BY x.made_on, x.id`)
	if err != nil {
		a.fail(w, err)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="oink.csv"`)
	w.Write([]byte("\xEF\xBB\xBF")) // BOM para que Excel abra UTF-8 bien
	cw := csv.NewWriter(w)
	cw.Write([]string{"fecha", "inicio_ciclo", "tipo", "rubro", "concepto", "monto"})
	for rows.Next() {
		var made, started time.Time
		var kind, category, concept string
		var credit, cash bool
		var amount int64
		if rows.Scan(&made, &started, &kind, &credit, &cash, &category, &concept, &amount) != nil {
			continue
		}
		tipo := csvKind(kind, credit)
		if cash {
			switch kind {
			case "income":
				tipo = "entrada_efectivo"
			case "fixed":
				tipo = "gasto_fijo_efectivo"
			}
		}
		cw.Write([]string{
			made.Format("2006-01-02"), started.Format("2006-01-02"),
			tipo, category, concept,
			fmt.Sprintf("%d.%02d", amount/100, amount%100),
		})
	}
	cw.Flush()
}

func csvKind(k string, credit bool) string {
	switch k {
	case "card":
		if credit {
			return "t_credito"
		}
		return "t_debito"
	case "cash":
		return "efectivo"
	case "withdrawal":
		return "retiro"
	case "income":
		return "entrada"
	case "fixed":
		return "gasto_fijo"
	case "cardpay":
		return "pago_tarjeta"
	case "loan_out":
		return "prestamo_salida"
	case "loan_in":
		return "prestamo_entrada"
	}
	return k
}

// ---- ajustes ----

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	s, err := a.getSettings()
	if err != nil {
		a.fail(w, err)
		return
	}
	c, _ := a.openCycle()
	tpls, err := a.listTemplates("fixed", c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	cats, _ := a.listTodoCats()
	a.render(w, "settings.html", map[string]any{"Nav": "ajustes", "S": s, "Cycle": c,
		"Templates": tpls, "TodoCats": cats,
		"Goal":      strconv.FormatInt(s.SavingsGoal/100, 10),
		"Weekly":    strconv.FormatInt(s.WeeklyCash/100, 10)})
}

func (a *App) settingsPost(w http.ResponseWriter, r *http.Request) {
	goal, err1 := parseMoney(r.FormValue("savings_goal"))
	weekly, err2 := parseMoney(r.FormValue("weekly_cash"))
	if err1 != nil || err2 != nil || goal < 0 || weekly <= 0 {
		http.Error(w, "datos inválidos", 400)
		return
	}
	if _, err := a.db.Exec(`UPDATE settings SET savings_goal=$1, weekly_cash=$2 WHERE id=1`, goal, weekly); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ajustes", http.StatusSeeOther)
}

func (a *App) cycleClose(w http.ResponseWriter, r *http.Request) {
	if _, err := a.rolloverCycle(); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
