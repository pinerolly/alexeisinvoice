// Command invoiceapp serves a small web app to edit and print an invoice
// for Electroclima Pro Services, LLC. Data is persisted to invoice-data.json
// next to the executable so it survives restarts.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
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
	InvoiceDate         string `json:"invoiceDate"` // yyyy-mm-dd
	ClientName          string `json:"clientName"`
	Phone               string `json:"phone"`
	Email               string `json:"email"`
	Location            string `json:"location"`
	TimeIn              string `json:"timeIn"` // HH:MM 24h
	TimeOut             string `json:"timeOut"`
	Jobs                []Job  `json:"jobs"`
	NextJobID           int    `json:"nextJobId"`
	CurrentInvoiceID    int64  `json:"currentInvoiceId"` // 0 = new/unsaved draft
	TechnicianUsername  string `json:"technicianUsername"`
	TechnicianSignature string `json:"technicianSignature"` // data:image/png;base64,... from the signature pad
	CustomerSignature   string `json:"customerSignature"`   // data:image/png;base64,... from the signature pad
}

var (
	mu   sync.Mutex
	data InvoiceData
)

func blankInvoice() InvoiceData {
	return InvoiceData{
		InvoiceDate: time.Now().Format("2006-01-02"),
		Jobs:        []Job{{ID: 1, Description: "", Price: 0}},
		NextJobID:   2,
	}
}

func loadData() {
	b, err := os.ReadFile(dataFile)
	if err != nil {
		data = blankInvoice()
		return
	}
	var d InvoiceData
	if err := json.Unmarshal(b, &d); err != nil {
		data = blankInvoice()
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

// loadDotEnv reads KEY=VALUE pairs from a .env file next to the executable,
// if present. Existing environment variables always take precedence.
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return // .env is optional
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

var funcMap = template.FuncMap{
	"formatDate":   formatDateDisplay,
	"formatTime":   formatTimeDisplay,
	"money":        money,
	"priceInput":   priceInput,
	"total":        totalOf,
	"signatureURL": signatureURL,
}

// signatureURL marks a validated "data:image/png;base64,..." signature as a
// safe URL so html/template's URL sanitizer (which blocks non-http(s)/mailto
// schemes by default) doesn't replace it with "#ZgotmplZ" in <img src>.
func signatureURL(dataURL string) template.URL {
	if !isValidPNGDataURL(dataURL) {
		return ""
	}
	return template.URL(dataURL)
}

func mustTemplate(name string) *template.Template {
	return template.Must(template.New(name).Funcs(funcMap).ParseFS(templateFS, "templates/"+name, "templates/partials.html"))
}

// ---- Handlers ----

// currentCSRFToken returns the CSRF token for the caller's session, used to
// populate hidden form fields on authenticated pages.
func currentCSRFToken(r *http.Request) string {
	_, s := getSession(r)
	if s == nil {
		return ""
	}
	return s.csrfToken
}

// EditPageView is the template data for the main editor page.
type EditPageView struct {
	InvoiceData
	CSRFToken                  string
	IsAdmin                    bool
	Username                   string
	DisplayName                string
	TechnicianSignaturePreview string
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	technician, _ := getUserByUsername(currentUsername(r))
	mu.Lock()
	view := EditPageView{
		InvoiceData:                data,
		CSRFToken:                  currentCSRFToken(r),
		IsAdmin:                    isAdmin(r),
		Username:                   currentUsername(r),
		DisplayName:                currentDisplayName(r),
		TechnicianSignaturePreview: technician.Signature,
	}
	mu.Unlock()
	if err := mustTemplate("edit.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	data.InvoiceDate = r.FormValue("invDate")
	data.ClientName = r.FormValue("clientName")
	data.Phone = r.FormValue("phone")
	data.Email = r.FormValue("email")
	data.Location = r.FormValue("location")
	data.TimeIn = r.FormValue("timeIn")
	data.TimeOut = r.FormValue("timeOut")
	data.CustomerSignature = r.FormValue("customerSignature")

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
		if data.CustomerSignature == "" {
			http.Error(w, "Customer signature is required before printing.", http.StatusBadRequest)
			return
		}
		technician, err := getUserByUsername(currentUsername(r))
		if err != nil || technician.Signature == "" {
			http.Redirect(w, r, "/account/signature", http.StatusSeeOther)
			return
		}
		data.TechnicianUsername = technician.Username
		data.TechnicianSignature = technician.Signature
		clientID, err := findOrCreateClient(data.ClientName, data.Phone, data.Email)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		invoiceID, err := saveInvoiceRecord(data.CurrentInvoiceID, clientID, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data = blankInvoice()
		if err := saveData(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view", invoiceID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func resetHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	data = blankInvoice()
	if err := saveData(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// SignaturePageView is the template data for the one-time account signature setup page.
type SignaturePageView struct {
	CSRFToken   string
	Username    string
	DisplayName string
	IsAdmin     bool
	Missing     bool
	Signature   string
}

func accountSignatureHandler(w http.ResponseWriter, r *http.Request) {
	technician, _ := getUserByUsername(currentUsername(r))
	view := SignaturePageView{
		CSRFToken:   currentCSRFToken(r),
		Username:    currentUsername(r),
		DisplayName: currentDisplayName(r),
		IsAdmin:     isAdmin(r),
		Missing:     r.URL.Query().Get("missing") == "1",
		Signature:   technician.Signature,
	}
	if err := mustTemplate("account_signature.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func accountSignatureSaveHandler(w http.ResponseWriter, r *http.Request) {
	sig := r.FormValue("signature")
	if sig == "" {
		http.Redirect(w, r, "/account/signature?missing=1", http.StatusSeeOther)
		return
	}
	if err := setUserSignature(currentUsername(r), sig); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ClientsListView is the template data for the clients list page.
type ClientsListView struct {
	Clients     []ClientSummary
	Search      string
	CSRFToken   string
	IsAdmin     bool
	Username    string
	DisplayName string
}

func clientsListHandler(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("q")
	clients, err := listClients(search)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := ClientsListView{Clients: clients, Search: search, CSRFToken: currentCSRFToken(r), IsAdmin: isAdmin(r), Username: currentUsername(r), DisplayName: currentDisplayName(r)}
	if err := mustTemplate("clients_list.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ClientDetailView is the template data for a single client's history page.
type ClientDetailView struct {
	Client      Client
	Invoices    []InvoiceSummary
	CSRFToken   string
	IsAdmin     bool
	Username    string
	DisplayName string
}

func clientDetailHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	client, err := getClient(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	invoices, err := getClientInvoices(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := ClientDetailView{Client: client, Invoices: invoices, CSRFToken: currentCSRFToken(r), IsAdmin: isAdmin(r), Username: currentUsername(r), DisplayName: currentDisplayName(r)}
	if err := mustTemplate("client_detail.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// InvoiceViewPage is the template data for the read-only historical invoice page.
type InvoiceViewPage struct {
	InvoiceData
	InvoiceID   int64
	ClientID    int64
	Title       string
	CSRFToken   string
	EmailSent   bool
	EmailError  string
	IsAdmin     bool
	Username    string
	DisplayName string
}

func invoiceViewHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	inv, client, err := getInvoiceWithJobs(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	view := InvoiceViewPage{
		InvoiceData: inv,
		InvoiceID:   id,
		ClientID:    client.ID,
		Title:       clientTitle(client.Name, inv.InvoiceDate),
		CSRFToken:   currentCSRFToken(r),
		EmailSent:   r.URL.Query().Get("emailed") == "1",
		EmailError:  r.URL.Query().Get("emailerror"),
		IsAdmin:     isAdmin(r),
		Username:    currentUsername(r),
		DisplayName: currentDisplayName(r),
	}
	if err := mustTemplate("invoice_view.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// invoiceDeleteHandler permanently removes an invoice and its jobs.
func invoiceDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, client, err := getInvoiceWithJobs(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := deleteInvoiceRecord(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/clients/%d", client.ID), http.StatusSeeOther)
}

// invoiceDownloadHandler streams the invoice as a downloadable PDF.
func invoiceDownloadHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	inv, client, err := getInvoiceWithJobs(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	title := clientTitle(client.Name, inv.InvoiceDate)
	pdfBytes, err := generateInvoicePDF(inv, client, title, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+invoiceFilename(title)+`"`)
	w.Write(pdfBytes)
}

// invoiceEmailHandler generates the invoice PDF and emails it to the address
// submitted in the form, then redirects back to the invoice view page.
func invoiceEmailHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	inv, client, err := getInvoiceWithJobs(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	to := strings.TrimSpace(r.FormValue("to"))
	redirectWithError := func(msg string) {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?emailerror=%s", id, url.QueryEscape(msg)), http.StatusSeeOther)
	}
	if to == "" || !strings.Contains(to, "@") {
		redirectWithError("Please enter a valid email address.")
		return
	}

	title := clientTitle(client.Name, inv.InvoiceDate)
	pdfBytes, err := generateInvoicePDF(inv, client, title, id)
	if err != nil {
		redirectWithError("Could not generate the PDF.")
		return
	}

	subject := fmt.Sprintf("Invoice from Electroclima Pro Services - %s", formatDateDisplay(inv.InvoiceDate))
	body := fmt.Sprintf("Hello %s,\n\nPlease find attached your invoice from Electroclima Pro Services, LLC.\n\nTotal Amount Due: %s\n\nThank you for your business!\n\nElectroclima Pro Services, LLC\n786 389 3330", inv.ClientName, money(totalOf(inv.Jobs)))
	if err := sendInvoiceEmail(to, subject, body, pdfBytes, invoiceFilename(title)); err != nil {
		redirectWithError("Could not send the email: " + err.Error())
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?emailed=1", id), http.StatusSeeOther)
}

func invoiceEditHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	inv, _, err := getInvoiceWithJobs(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	inv.CurrentInvoiceID = id

	mu.Lock()
	data = inv
	err = saveData()
	mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func main() {
	hashPassword := flag.Bool("hashpassword", false, "prompt for a password and print its bcrypt hash, then exit")
	flag.Parse()

	if *hashPassword {
		fmt.Print("Enter password: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			log.Fatal(err)
		}
		hash, err := bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(hash))
		return
	}

	loadDotEnv(".env")
	loadData()
	if err := initDB(); err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := seedDefaultAdmin(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", loginHandler)
	mux.HandleFunc("POST /login", loginHandler)
	mux.HandleFunc("POST /logout", logoutHandler)

	mux.HandleFunc("GET /{$}", requireAuth(requireSignature(indexHandler)))
	mux.HandleFunc("POST /update", requireCSRF(requireSignature(updateHandler)))
	mux.HandleFunc("POST /reset", requireCSRF(requireSignature(resetHandler)))
	mux.HandleFunc("GET /clients", requireAuth(requireSignature(clientsListHandler)))
	mux.HandleFunc("GET /clients/{id}", requireAuth(requireSignature(clientDetailHandler)))
	mux.HandleFunc("GET /invoices/{id}/view", requireAuth(requireSignature(invoiceViewHandler)))
	mux.HandleFunc("GET /invoices/{id}/download", requireAuth(requireSignature(invoiceDownloadHandler)))
	mux.HandleFunc("POST /invoices/{id}/email", requireCSRF(requireSignature(invoiceEmailHandler)))
	mux.HandleFunc("GET /invoices/{id}/edit", requireAuth(requireSignature(invoiceEditHandler)))
	mux.HandleFunc("POST /invoices/{id}/delete", requireAdmin(requireCSRF(invoiceDeleteHandler)))
	mux.HandleFunc("GET /account/signature", requireAuth(accountSignatureHandler))
	mux.HandleFunc("POST /account/signature", requireCSRF(accountSignatureSaveHandler))
	mux.HandleFunc("POST /account", requireAuth(requireCSRF(accountPasswordHandler)))
	mux.HandleFunc("GET /admin/users", requireAuth(usersListHandler))
	mux.HandleFunc("POST /admin/users", requireAdmin(requireCSRF(usersCreateHandler)))
	mux.HandleFunc("POST /admin/users/{id}/delete", requireAdmin(requireCSRF(usersDeleteHandler)))
	mux.HandleFunc("POST /admin/users/{id}/role", requireAdmin(requireCSRF(usersRoleHandler)))
	mux.HandleFunc("POST /admin/users/{id}/name", requireAdmin(requireCSRF(usersNameHandler)))

	addr := ":8080"
	log.Printf("Invoice app running at http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
