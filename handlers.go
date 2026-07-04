package main

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) parseTemplates() {
	funcs := template.FuncMap{
		"money": fmtMoney,
		"date":  fmtDate,
		"cycle": fmtCycle,
		"month": fmtMonth,
		"amt": func(cents int64) string {
			if cents%100 == 0 {
				return strconv.FormatInt(cents/100, 10)
			}
			return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
		},
		"kindLabel": func(k string, credit bool) string {
			switch k {
			case "card":
				if credit {
					return "Tarjeta · crédito"
				}
				return "Tarjeta · débito"
			case "cash":
				return "Efectivo"
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
	pages := []string{"login.html", "home.html", "incomes.html", "fixed.html", "month.html", "reportes.html", "settings.html", "txedit.html"}
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
		"CardConcepts": a.recentConcepts("card"),
		"CashConcepts": a.recentConcepts("cash"),
	})
}

// ---- transacciones ----

func (a *App) txCreate(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	if kind != "card" && kind != "cash" && kind != "withdrawal" {
		http.Error(w, "tipo inválido", 400)
		return
	}
	amount, err := parseMoney(r.FormValue("amount"))
	if err != nil || amount <= 0 {
		http.Error(w, "monto inválido", 400)
		return
	}
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	concept := strings.TrimSpace(r.FormValue("concept"))
	credit := kind == "card" && r.FormValue("credit") != "off"
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, amount, concept, credit, made_on)
		VALUES ($1,$2,$3,$4,$5,$6)`, c.ID, kind, amount, concept, credit, a.today())
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) txEditPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var t Tx
	err := a.db.QueryRow(`SELECT x.id, x.kind, x.amount, x.concept, x.credit, x.made_on, t.name
		FROM transactions x LEFT JOIN templates t ON t.id = x.template_id WHERE x.id = $1`, id).
		Scan(&t.ID, &t.Kind, &t.Amount, &t.Concept, &t.Credit, &t.MadeOn, &t.TplName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, "txedit.html", map[string]any{"Nav": "mes", "T": t,
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
	concept := strings.TrimSpace(r.FormValue("concept"))
	credit := r.FormValue("credit") == "on"
	_, err = a.db.Exec(`UPDATE transactions SET amount=$1, concept=$2, made_on=$3,
		credit = CASE WHEN kind='card' THEN $4 ELSE credit END WHERE id=$5`,
		amount, concept, madeOn, credit, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/mes", http.StatusSeeOther)
}

func (a *App) txDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	a.db.Exec(`DELETE FROM transactions WHERE id = $1`, id)
	http.Redirect(w, r, "/mes", http.StatusSeeOther)
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
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, amount, concept, made_on)
		VALUES ($1,'income',$2,$3,$4)`, c.ID, amount, concept, a.today())
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

// ---- fijos ----

func (a *App) fixedPage(w http.ResponseWriter, r *http.Request) {
	c, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	tpls, err := a.listTemplates("fixed", c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, "fixed.html", map[string]any{"Nav": "fijos", "Templates": tpls, "Cycle": c})
}

func (a *App) fixedPay(w http.ResponseWriter, r *http.Request) {
	tplID, _ := strconv.Atoi(r.FormValue("template_id"))
	var tpl Template
	err := a.db.QueryRow(`SELECT id, name, amount FROM templates WHERE id = $1 AND kind = 'fixed' AND active`, tplID).
		Scan(&tpl.ID, &tpl.Name, &tpl.Amount)
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
	_, err = a.db.Exec(`INSERT INTO transactions (cycle_id, kind, template_id, amount, concept, made_on)
		VALUES ($1,'fixed',$2,$3,$4,$5)`, c.ID, tpl.ID, amount, tpl.Name, a.today())
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/fijos", http.StatusSeeOther)
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
	a.db.Exec(`INSERT INTO templates (kind, name, amount) VALUES ($1,$2,$3)`, kind, name, amount)
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
	if err := a.db.QueryRow(`UPDATE templates SET name=$1, amount=$2 WHERE id=$3 RETURNING kind`,
		name, amount, id).Scan(&kind); err != nil {
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
		http.Redirect(w, r, "/fijos", http.StatusSeeOther)
	}
}

// ---- mes / historial ----

func (a *App) monthPage(w http.ResponseWriter, r *http.Request) {
	cur, err := a.openCycle()
	if err != nil {
		a.fail(w, err)
		return
	}
	c := cur
	if v := r.URL.Query().Get("c"); v != "" {
		id, _ := strconv.Atoi(v)
		err := a.db.QueryRow(`SELECT id, started_on, closed_on FROM cycles WHERE id = $1`, id).
			Scan(&c.ID, &c.StartedOn, &c.ClosedOn)
		if err != nil {
			c = cur
		}
	}
	txs, err := a.listTx(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	var prev, next sql.NullInt64
	a.db.QueryRow(`SELECT id FROM cycles WHERE (started_on, id) < ($1, $2) ORDER BY started_on DESC, id DESC LIMIT 1`,
		c.StartedOn, c.ID).Scan(&prev)
	a.db.QueryRow(`SELECT id FROM cycles WHERE (started_on, id) > ($1, $2) ORDER BY started_on ASC, id ASC LIMIT 1`,
		c.StartedOn, c.ID).Scan(&next)

	incomes := a.sumTx(c.ID, `kind = 'income'`)
	fixed := a.sumTx(c.ID, `kind = 'fixed'`)
	card := a.sumTx(c.ID, `kind = 'card'`)
	creditCard := a.sumTx(c.ID, `kind = 'card' AND credit`)
	withdrawals := a.sumTx(c.ID, `kind = 'withdrawal'`)
	cash := a.sumTx(c.ID, `kind = 'cash'`)
	saved := incomes - fixed - card - withdrawals

	a.render(w, "month.html", map[string]any{
		"Nav": "mes", "C": c, "Txs": txs, "IsCurrent": c.ID == cur.ID,
		"Prev": prev, "Next": next,
		"Incomes": incomes, "Fixed": fixed, "Card": card, "CreditCard": creditCard,
		"Withdrawals": withdrawals, "Cash": cash, "Saved": saved,
	})
}

// ---- reportes ----

func (a *App) reportsPage(w http.ResponseWriter, r *http.Request) {
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
	fixed := a.sumTx(c.ID, `kind = 'fixed'`)
	card := a.sumTx(c.ID, `kind = 'card'`)
	withdrawals := a.sumTx(c.ID, `kind = 'withdrawal'`)
	spent := fixed + card + withdrawals
	saved := incomes - spent

	concepts, conceptTotal := a.conceptBreakdown(c.ID)
	history, err := a.cycleHistory(12)
	if err != nil {
		a.fail(w, err)
		return
	}

	a.render(w, "reportes.html", map[string]any{
		"Nav": "reportes", "C": c, "IsCurrent": c.ID == cur.ID,
		"Prev": prev, "Next": next,
		"Incomes": incomes, "Fixed": fixed, "Card": card, "Withdrawals": withdrawals,
		"Spent": spent, "Saved": saved,
		"Concepts": concepts, "ConceptTotal": conceptTotal, "History": history,
	})
}

// ---- ajustes ----

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	s, err := a.getSettings()
	if err != nil {
		a.fail(w, err)
		return
	}
	c, _ := a.openCycle()
	a.render(w, "settings.html", map[string]any{"Nav": "ajustes", "S": s, "Cycle": c,
		"Goal":   strconv.FormatInt(s.SavingsGoal/100, 10),
		"Weekly": strconv.FormatInt(s.WeeklyCash/100, 10)})
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
