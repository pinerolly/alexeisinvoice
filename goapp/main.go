// Command invoiceapp serves a small web app to edit and print an invoice
// for Electroclima Pro Services, LLC. Data is persisted to invoice-data.json
// next to the executable so it survives restarts.
package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const maxEvidencePhotoBytes = 10 << 20 // 10 MiB
const defaultEvidenceZipEmailMaxBytes = 20 << 20

func evidenceZipEmailMaxBytes() int {
	raw := strings.TrimSpace(os.Getenv("EVIDENCE_ZIP_EMAIL_MAX_BYTES"))
	if raw == "" {
		return defaultEvidenceZipEmailMaxBytes
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return defaultEvidenceZipEmailMaxBytes
	}
	if v > int64(^uint(0)>>1) {
		return defaultEvidenceZipEmailMaxBytes
	}
	return int(v)
}

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
	PaymentStatus       string `json:"paymentStatus"`       // "unpaid", "partial", or "paid"
	AmountPaid          float64 `json:"amountPaid"`         // amount paid so far when PaymentStatus is "partial"
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

func formatTimestampDisplay(ts string) string {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return parsed.Local().Format("01/02/2006 3:04 PM")
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
	"formatTS":     formatTimestampDisplay,
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
	if dir := strings.TrimSpace(os.Getenv("TEMPLATE_DIR")); dir != "" {
		return template.Must(template.New(name).Funcs(funcMap).ParseFiles(
			filepath.Join(dir, name),
			filepath.Join(dir, "partials.html"),
		))
	}
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

type PublicPageView struct {
	Title          string
	RequestSent    bool
	RequestError   string
	RequestName    string
	RequestPhone   string
	RequestEmail   string
	RequestService string
	RequestMessage string
}

func publicHandler(w http.ResponseWriter, r *http.Request) {
	view := PublicPageView{
		Title:          "Electroclima Pro Services, LLC",
		RequestSent:    r.URL.Query().Get("request") == "1",
		RequestError:   r.URL.Query().Get("requesterror"),
		RequestName:    r.URL.Query().Get("name"),
		RequestPhone:   r.URL.Query().Get("phone"),
		RequestEmail:   r.URL.Query().Get("email"),
		RequestService: r.URL.Query().Get("service"),
		RequestMessage: r.URL.Query().Get("message"),
	}
	if err := mustTemplate("public_home.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func serviceRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/?requesterror="+url.QueryEscape("Could not submit request. Please try again.")+"#contact", http.StatusSeeOther)
		return
	}
	// Honeypot: bots often fill hidden fields; humans do not.
	if strings.TrimSpace(r.FormValue("website")) != "" {
		http.Redirect(w, r, "/?request=1#contact", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	phone := strings.TrimSpace(r.FormValue("phone"))
	email := strings.TrimSpace(r.FormValue("email"))
	serviceType := strings.TrimSpace(r.FormValue("service_type"))
	message := strings.TrimSpace(r.FormValue("message"))

	q := url.Values{}
	q.Set("name", name)
	q.Set("phone", phone)
	q.Set("email", email)
	q.Set("service", serviceType)
	q.Set("message", message)

	if name == "" {
		q.Set("requesterror", "Please enter your name.")
		http.Redirect(w, r, "/?"+q.Encode()+"#contact", http.StatusSeeOther)
		return
	}
	if phone == "" && email == "" {
		q.Set("requesterror", "Please provide a phone number or email.")
		http.Redirect(w, r, "/?"+q.Encode()+"#contact", http.StatusSeeOther)
		return
	}
	if len(message) > 3000 {
		q.Set("requesterror", "Message is too long. Please keep it under 3000 characters.")
		http.Redirect(w, r, "/?"+q.Encode()+"#contact", http.StatusSeeOther)
		return
	}

	if _, err := createServiceRequest(name, phone, email, serviceType, message); err != nil {
		q.Set("requesterror", "Could not save your request right now. Please try again.")
		http.Redirect(w, r, "/?"+q.Encode()+"#contact", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/?request=1#contact", http.StatusSeeOther)
}

type ServiceRequestsPageView struct {
	Title       string
	Requests    []ServiceRequest
	CSRFToken   string
	IsAdmin     bool
	Username    string
	DisplayName string
}

func serviceRequestsHandler(w http.ResponseWriter, r *http.Request) {
	reqs, err := listServiceRequests(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := ServiceRequestsPageView{
		Title:       "Service Requests",
		Requests:    reqs,
		CSRFToken:   currentCSRFToken(r),
		IsAdmin:     isAdmin(r),
		Username:    currentUsername(r),
		DisplayName: currentDisplayName(r),
	}
	if err := mustTemplate("service_requests.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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
	Client        Client
	Invoices      []InvoiceSummary
	StatusFilter  string
	LifetimeTotal float64
	TotalDebt     float64
	CSRFToken     string
	IsAdmin       bool
	Username      string
	DisplayName   string
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
	statusFilter := r.URL.Query().Get("status")
	if !validPaymentStatuses[statusFilter] {
		statusFilter = ""
	}
	invoices, err := getClientInvoices(id, statusFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lifetimeTotal, err := getClientLifetimeTotal(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totalDebt, err := getClientTotalDebt(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := ClientDetailView{Client: client, Invoices: invoices, StatusFilter: statusFilter, LifetimeTotal: lifetimeTotal, TotalDebt: totalDebt, CSRFToken: currentCSRFToken(r), IsAdmin: isAdmin(r), Username: currentUsername(r), DisplayName: currentDisplayName(r)}
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
	Photos      []InvoicePhoto
	CSRFToken   string
	EmailSent   bool
	EmailError  string
	EmailWarn   string
	PhotoError  string
	EmailTo     string
	DefaultCC   string
	ShowCC      bool
	AttachZip   bool
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
		EmailWarn:   r.URL.Query().Get("emailwarn"),
		PhotoError:  r.URL.Query().Get("photoerror"),
		EmailTo:     r.URL.Query().Get("to"),
		DefaultCC:   r.URL.Query().Get("cc"),
		ShowCC:      r.URL.Query().Get("ccopen") == "1",
		AttachZip:   r.URL.Query().Get("zip") == "1",
		IsAdmin:     isAdmin(r),
		Username:    currentUsername(r),
		DisplayName: currentDisplayName(r),
	}
	photos, err := listInvoicePhotos(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.Photos = photos
	if _, hasZip := r.URL.Query()["zip"]; !hasZip && len(view.Photos) > 0 {
		view.AttachZip = true
	}
	if view.EmailTo == "" {
		view.EmailTo = inv.Email
	}
	if view.DefaultCC != "" {
		view.ShowCC = true
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

// validPaymentStatuses lists the allowed values for an invoice's payment_status.
var validPaymentStatuses = map[string]bool{"unpaid": true, "partial": true, "paid": true}

// invoicePaidHandler updates an invoice's payment status (admin only).
func invoicePaidHandler(w http.ResponseWriter, r *http.Request) {
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
	status := r.FormValue("payment_status")
	if !validPaymentStatuses[status] {
		http.Error(w, "invalid payment status", http.StatusBadRequest)
		return
	}
	amountPaid, _ := strconv.ParseFloat(r.FormValue("amount_paid"), 64)
	if err := setInvoicePaymentStatus(id, status, amountPaid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = fmt.Sprintf("/clients/%d", client.ID)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// invoiceAddPaymentHandler records an additional installment payment on an
// invoice, adding to whatever has already been paid (admin only).
func invoiceAddPaymentHandler(w http.ResponseWriter, r *http.Request) {
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
	amount, err := strconv.ParseFloat(r.FormValue("amount"), 64)
	if err != nil || amount <= 0 {
		http.Error(w, "invalid payment amount", http.StatusBadRequest)
		return
	}
	if err := addInvoicePayment(id, amount); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = fmt.Sprintf("/clients/%d", client.ID)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
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
	cc := strings.TrimSpace(r.FormValue("cc"))
	ccOpen := r.FormValue("cc_open") == "1"
	attachZip := r.FormValue("attach_evidence_zip") == "1"
	emailWarn := ""
	redirectWithError := func(msg string) {
		q := url.Values{}
		q.Set("emailerror", msg)
		q.Set("to", to)
		q.Set("cc", cc)
		if attachZip {
			q.Set("zip", "1")
		}
		if ccOpen || cc != "" {
			q.Set("ccopen", "1")
		}
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?%s", id, q.Encode()), http.StatusSeeOther)
	}
	if to == "" || !strings.Contains(to, "@") {
		redirectWithError("Please enter a valid email address.")
		return
	}
	if cc != "" && !strings.Contains(cc, "@") {
		redirectWithError("Please enter a valid CC email address.")
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
	attachments := []emailAttachment{{
		Filename:    invoiceFilename(title),
		ContentType: "application/pdf",
		Data:        pdfBytes,
	}}
	if attachZip {
		photos, err := listInvoicePhotos(id)
		if err != nil {
			redirectWithError("Could not load evidence photos.")
			return
		}
		zipBytes, zipName, err := buildInvoiceEvidenceZip(id, photos)
		if err != nil {
			redirectWithError("Could not attach evidence ZIP: " + err.Error())
			return
		}
		maxZip := evidenceZipEmailMaxBytes()
		if len(zipBytes) > maxZip {
			emailWarn = fmt.Sprintf("Evidence ZIP is too large (%s). Sent invoice PDF without ZIP. Limit is %s.", humanBytes(int64(len(zipBytes))), humanBytes(int64(maxZip)))
		} else {
			attachments = append(attachments, emailAttachment{
				Filename:    zipName,
				ContentType: "application/zip",
				Data:        zipBytes,
			})
		}
	}
	if err := sendInvoiceEmailWithAttachments(to, cc, subject, body, attachments); err != nil {
		redirectWithError("Could not send the email: " + err.Error())
		return
	}

	q := url.Values{}
	q.Set("emailed", "1")
	q.Set("to", to)
	q.Set("cc", cc)
	if emailWarn != "" {
		q.Set("emailwarn", emailWarn)
	}
	if attachZip {
		q.Set("zip", "1")
	}
	if ccOpen || cc != "" {
		q.Set("ccopen", "1")
	}
	http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?%s", id, q.Encode()), http.StatusSeeOther)
}

func humanBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := int64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(v)/float64(div), "KMGTPE"[exp])
}

func buildInvoiceEvidenceZip(invoiceID int64, photos []InvoicePhoto) ([]byte, string, error) {
	if len(photos) == 0 {
		return nil, "", fmt.Errorf("no evidence photos attached")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for i, p := range photos {
		rc, _, err := openInvoicePhotoObject(p.ObjectKey)
		if err != nil {
			zw.Close()
			return nil, "", err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			zw.Close()
			return nil, "", err
		}

		name := sanitizeZipEntryName(p.Filename)
		if name == "" {
			name = fmt.Sprintf("photo_%d.jpg", i+1)
		}
		entryName := fmt.Sprintf("%03d_%s", i+1, name)
		w, err := zw.Create(entryName)
		if err != nil {
			zw.Close()
			return nil, "", err
		}
		if _, err := w.Write(content); err != nil {
			zw.Close()
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), fmt.Sprintf("invoice_%d_evidence.zip", invoiceID), nil
}

func sanitizeZipEntryName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "..", "_")
	return name
}

func invoicePhotoUploadHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, err := getInvoiceWithJobs(id); err != nil {
		http.NotFound(w, r)
		return
	}

	redirectWithError := func(msg string) {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape(msg)), http.StatusSeeOther)
	}

	if err := r.ParseMultipartForm(maxEvidencePhotoBytes + (1 << 20)); err != nil {
		redirectWithError("Could not read uploaded file.")
		return
	}
	headers := r.MultipartForm.File["photo"]
	if len(headers) == 0 {
		redirectWithError("Please select a photo file to upload.")
		return
	}
	uploader := currentUsername(r)
	success := 0
	failures := 0

	for _, header := range headers {
		file, err := header.Open()
		if err != nil {
			failures++
			continue
		}
		content, err := io.ReadAll(io.LimitReader(file, maxEvidencePhotoBytes+1))
		file.Close()
		if err != nil {
			failures++
			continue
		}
		if len(content) == 0 || len(content) > maxEvidencePhotoBytes {
			failures++
			continue
		}

		contentType := http.DetectContentType(content)
		switch contentType {
		case "image/jpeg", "image/png", "image/webp":
		default:
			failures++
			continue
		}

		objectKey, err := uploadInvoicePhoto(id, header.Filename, contentType, content)
		if err != nil {
			failures++
			continue
		}
		if _, err := createInvoicePhoto(id, objectKey, sanitizeFilename(header.Filename), contentType, uploader, int64(len(content))); err != nil {
			_ = deleteInvoicePhotoObject(objectKey)
			failures++
			continue
		}
		success++
	}

	if success == 0 {
		redirectWithError("No photos were uploaded. Make sure files are JPG, PNG, or WEBP and under 10 MB each.")
		return
	}
	if failures > 0 {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape(fmt.Sprintf("Uploaded %d photos; %d failed validation or upload.", success, failures))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view", id), http.StatusSeeOther)
}

func invoicePhotoViewHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	photoID, err := strconv.ParseInt(r.PathValue("photoID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	photo, err := getInvoicePhoto(photoID)
	if err != nil || photo.InvoiceID != id {
		http.NotFound(w, r)
		return
	}
	body, detectedContentType, err := openInvoicePhotoObject(photo.ObjectKey)
	if err != nil {
		http.Error(w, "could not open photo", http.StatusInternalServerError)
		return
	}
	defer body.Close()

	contentType := detectedContentType
	if contentType == "" {
		contentType = photo.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", photo.Filename))
	if _, err := io.Copy(w, body); err != nil {
		http.Error(w, "could not stream photo", http.StatusInternalServerError)
	}
}

func invoicePhotoDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	photoID, err := strconv.ParseInt(r.PathValue("photoID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	photo, err := getInvoicePhoto(photoID)
	if err != nil || photo.InvoiceID != id {
		http.NotFound(w, r)
		return
	}

	_ = deleteInvoicePhotoObject(photo.ObjectKey)
	if err := deleteInvoicePhoto(photoID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view", id), http.StatusSeeOther)
}

func invoicePhotosZipHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, err := getInvoiceWithJobs(id); err != nil {
		http.NotFound(w, r)
		return
	}
	photos, err := listInvoicePhotos(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	zipBytes, zipName, err := buildInvoiceEvidenceZip(id, photos)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape("No evidence photos available to download.")), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", zipName))
	w.Write(zipBytes)
}

func selectedInvoicePhotos(r *http.Request, invoiceID int64) ([]InvoicePhoto, error) {
	rawIDs := r.Form["photo_ids"]
	if len(rawIDs) == 0 {
		return nil, nil
	}
	seen := map[int64]bool{}
	photos := make([]InvoicePhoto, 0, len(rawIDs))
	for _, raw := range rawIDs {
		photoID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || photoID <= 0 || seen[photoID] {
			continue
		}
		seen[photoID] = true
		photo, err := getInvoicePhoto(photoID)
		if err != nil || photo.InvoiceID != invoiceID {
			continue
		}
		photos = append(photos, photo)
	}
	return photos, nil
}

func invoicePhotosSelectedZipHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, err := getInvoiceWithJobs(id); err != nil {
		http.NotFound(w, r)
		return
	}
	photos, err := selectedInvoicePhotos(r, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(photos) == 0 {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape("Please select at least one photo to download.")), http.StatusSeeOther)
		return
	}
	zipBytes, _, err := buildInvoiceEvidenceZip(id, photos)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape("Could not build selected photos ZIP.")), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("invoice_%d_selected_photos.zip", id)))
	w.Write(zipBytes)
}

func invoicePhotosDeleteSelectedHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, err := getInvoiceWithJobs(id); err != nil {
		http.NotFound(w, r)
		return
	}
	photos, err := selectedInvoicePhotos(r, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(photos) == 0 {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape("Please select at least one photo to delete.")), http.StatusSeeOther)
		return
	}

	deleted := 0
	failures := 0
	for _, photo := range photos {
		_ = deleteInvoicePhotoObject(photo.ObjectKey)
		if err := deleteInvoicePhoto(photo.ID); err != nil {
			failures++
			continue
		}
		deleted++
	}

	if deleted == 0 {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape("Could not delete selected photos.")), http.StatusSeeOther)
		return
	}
	if failures > 0 {
		http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view?photoerror=%s", id, url.QueryEscape(fmt.Sprintf("Deleted %d photos; %d could not be deleted.", deleted, failures))), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/invoices/%d/view", id), http.StatusSeeOther)
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
	http.Redirect(w, r, "/app", http.StatusSeeOther)
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
	mux.HandleFunc("GET /{$}", publicHandler)
	mux.HandleFunc("POST /service-request", serviceRequestHandler)
	mux.HandleFunc("GET /login", loginHandler)
	mux.HandleFunc("POST /login", loginHandler)
	mux.HandleFunc("POST /logout", logoutHandler)

	mux.HandleFunc("GET /app", requireAuth(requireSignature(indexHandler)))
	mux.HandleFunc("GET /admin/service-requests", requireAuth(serviceRequestsHandler))
	mux.HandleFunc("POST /update", requireCSRF(requireSignature(updateHandler)))
	mux.HandleFunc("POST /reset", requireCSRF(requireSignature(resetHandler)))
	mux.HandleFunc("GET /clients", requireAuth(requireSignature(clientsListHandler)))
	mux.HandleFunc("GET /clients/{id}", requireAuth(requireSignature(clientDetailHandler)))
	mux.HandleFunc("GET /invoices/{id}/view", requireAuth(requireSignature(invoiceViewHandler)))
	mux.HandleFunc("GET /invoices/{id}/download", requireAuth(requireSignature(invoiceDownloadHandler)))
	mux.HandleFunc("POST /invoices/{id}/email", requireCSRF(requireSignature(invoiceEmailHandler)))
	mux.HandleFunc("POST /invoices/{id}/photos", requireCSRF(requireSignature(invoicePhotoUploadHandler)))
	mux.HandleFunc("GET /invoices/{id}/photos/{photoID}", requireAuth(requireSignature(invoicePhotoViewHandler)))
	mux.HandleFunc("POST /invoices/{id}/photos/{photoID}/delete", requireCSRF(requireSignature(invoicePhotoDeleteHandler)))
	mux.HandleFunc("GET /invoices/{id}/photos.zip", requireAuth(requireSignature(invoicePhotosZipHandler)))
	mux.HandleFunc("POST /invoices/{id}/photos/download-selected", requireCSRF(requireSignature(invoicePhotosSelectedZipHandler)))
	mux.HandleFunc("POST /invoices/{id}/photos/delete-selected", requireCSRF(requireSignature(invoicePhotosDeleteSelectedHandler)))
	mux.HandleFunc("GET /invoices/{id}/edit", requireAuth(requireSignature(invoiceEditHandler)))
	mux.HandleFunc("POST /invoices/{id}/delete", requireAdmin(requireCSRF(invoiceDeleteHandler)))
	mux.HandleFunc("POST /invoices/{id}/paid", requireAdmin(requireCSRF(invoicePaidHandler)))
	mux.HandleFunc("POST /invoices/{id}/add-payment", requireAdmin(requireCSRF(invoiceAddPaymentHandler)))
	mux.HandleFunc("GET /account/signature", requireAuth(accountSignatureHandler))
	mux.HandleFunc("POST /account/signature", requireCSRF(accountSignatureSaveHandler))
	mux.HandleFunc("POST /account", requireAuth(requireCSRF(accountPasswordHandler)))
	mux.HandleFunc("GET /admin/users", requireAuth(usersListHandler))
	mux.HandleFunc("POST /admin/users", requireAdmin(requireCSRF(usersCreateHandler)))
	mux.HandleFunc("POST /admin/users/{id}/delete", requireAdmin(requireCSRF(usersDeleteHandler)))
	mux.HandleFunc("POST /admin/users/{id}/role", requireAdmin(requireCSRF(usersRoleHandler)))
	mux.HandleFunc("POST /admin/users/{id}/name", requireAdmin(requireCSRF(usersNameHandler)))
	mux.HandleFunc("GET /admin/bank", requireAdmin(plaidBankPageHandler))
	mux.HandleFunc("POST /admin/plaid/link-token", requireAdmin(requireCSRF(plaidLinkTokenHandler)))
	mux.HandleFunc("POST /admin/plaid/exchange", requireAdmin(requireCSRF(plaidExchangeHandler)))
	mux.HandleFunc("POST /admin/plaid/{id}/delete", requireAdmin(requireCSRF(plaidDeleteHandler)))

	addr := ":8080"
	log.Printf("Invoice app running at http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
