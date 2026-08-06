// Command invoiceapp serves a small web app to edit and print an invoice
// for Electroclima Pro Services, LLC. Data is persisted to invoice-data.json
// next to the executable so it survives restarts.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

//go:embed templates/*.html
var templateFS embed.FS

const dataFile = "invoice-data.json"

// Job is a single description/price line item on the invoice.
type Job struct {
	ID          int     `json:"id"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
}

// InvoiceData holds all editable fields of the invoice.
type InvoiceData struct {
	InvoiceDate string `json:"invoiceDate"` // yyyy-mm-dd
	ClientName  string `json:"clientName"`
	Location    string `json:"location"`
	TimeIn      string `json:"timeIn"` // HH:MM 24h
	TimeOut     string `json:"timeOut"`
	Jobs        []Job  `json:"jobs"`
	NextJobID   int    `json:"nextJobId"`
}

var (
	mu   sync.Mutex
	data InvoiceData
)

func defaultData() InvoiceData {
	return InvoiceData{
		InvoiceDate: "2026-07-18",
		ClientName:  "",
		Location:    "7820 NE 1st Ave",
		TimeIn:      "10:00",
		TimeOut:     "13:30",
		Jobs: []Job{
			{
				ID: 1,
				Description: "The A/C unit is working properly, but the return duct was too small, restricting proper airflow and cooling.\n\n" +
					"Installed a new 14\" x 14\" return grille connected to a 12\" duct, tied into the unit's return line. System tested and confirmed working correctly.",
				Price: 500,
			},
		},
		NextJobID: 2,
	}
}

func loadData() {
	b, err := os.ReadFile(dataFile)
	if err != nil {
		data = defaultData()
		return
	}
	var d InvoiceData
	if err := json.Unmarshal(b, &d); err != nil {
		data = defaultData()
		return
	}
	data = d
}

func saveData() error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dataFile, b, 0644)
}

// ---- Template helpers ----

func formatDateDisplay(iso string) string {
	parts := strings.Split(iso, "-")
	if len(parts) != 3 {
		return iso
	}
	return parts[1] + "/" + parts[2] + "/" + parts[0]
}

func formatTimeDisplay(t string) string {
	parts := strings.Split(t, ":")
	if len(parts) != 2 {
		return t
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return t
	}
	period := "AM"
	if h >= 12 {
		period = "PM"
	}
	h12 := h % 12
	if h12 == 0 {
		h12 = 12
	}
	return fmt.Sprintf("%d:%s %s", h12, parts[1], period)
}

func money(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

func priceInput(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

func totalOf(jobs []Job) float64 {
	var t float64
	for _, j := range jobs {
		t += j.Price
	}
	return t
}

func slugify(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Invoice"
	}
	replacer := strings.NewReplacer(
		"/", "", "\\", "", ":", "", "*", "", "?", "", "\"", "", "<", "", ">", "", "|", "",
	)
	s = replacer.Replace(s)
	return strings.Join(strings.Fields(s), "_")
}

var funcMap = template.FuncMap{
	"formatDate": formatDateDisplay,
	"formatTime": formatTimeDisplay,
	"money":      money,
	"priceInput": priceInput,
	"total":      totalOf,
}

func mustTemplate(name string) *template.Template {
	return template.Must(template.New(name).Funcs(funcMap).ParseFS(templateFS, "templates/"+name))
}

// ---- Handlers ----

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if err := mustTemplate("edit.html").Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func printHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	view := struct {
		InvoiceData
		Title string
	}{data, slugify(data.ClientName) + "_" + data.InvoiceDate}
	if err := mustTemplate("print.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	data.InvoiceDate = r.FormValue("invDate")
	data.ClientName = r.FormValue("clientName")
	data.Location = r.FormValue("location")
	data.TimeIn = r.FormValue("timeIn")
	data.TimeOut = r.FormValue("timeOut")

	descs := r.Form["description"]
	prices := r.Form["price"]
	ids := r.Form["jobId"]

	jobs := make([]Job, 0, len(descs))
	for i := range descs {
		var price float64
		if i < len(prices) {
			price, _ = strconv.ParseFloat(strings.TrimSpace(prices[i]), 64)
		}
		id := 0
		if i < len(ids) {
			id, _ = strconv.Atoi(ids[i])
		}
		jobs = append(jobs, Job{ID: id, Description: descs[i], Price: price})
	}

	action := r.FormValue("action")
	switch {
	case action == "add":
		jobs = append(jobs, Job{ID: data.NextJobID, Description: "", Price: 0})
		data.NextJobID++
	case strings.HasPrefix(action, "remove:"):
		removeID, _ := strconv.Atoi(strings.TrimPrefix(action, "remove:"))
		filtered := make([]Job, 0, len(jobs))
		for _, j := range jobs {
			if j.ID != removeID {
				filtered = append(filtered, j)
			}
		}
		if len(filtered) == 0 {
			filtered = append(filtered, Job{ID: data.NextJobID, Description: "", Price: 0})
			data.NextJobID++
		}
		jobs = filtered
	}

	data.Jobs = jobs
	if err := saveData(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if action == "print" {
		http.Redirect(w, r, "/print", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func resetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	data = defaultData()
	if err := saveData(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func main() {
	loadData()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/update", updateHandler)
	mux.HandleFunc("/print", printHandler)
	mux.HandleFunc("/reset", resetHandler)

	addr := ":8080"
	log.Printf("Invoice app running at http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
