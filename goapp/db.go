// Database access for client and invoice history, backed by SQLite.
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const dbFile = "invoices.db"

var db *sql.DB

const schema = `
CREATE TABLE IF NOT EXISTS clients (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	phone TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invoices (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	client_id INTEGER NOT NULL REFERENCES clients(id),
	invoice_date TEXT NOT NULL,
	location TEXT NOT NULL DEFAULT '',
	time_in TEXT NOT NULL DEFAULT '',
	time_out TEXT NOT NULL DEFAULT '',
	total REAL NOT NULL DEFAULT 0,
	technician_username TEXT NOT NULL DEFAULT '',
	technician_signature TEXT NOT NULL DEFAULT '',
	customer_signature TEXT NOT NULL DEFAULT '',
	payment_status TEXT NOT NULL DEFAULT 'unpaid',
	amount_paid REAL NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invoice_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	invoice_id INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
	description TEXT NOT NULL DEFAULT '',
	price REAL NOT NULL DEFAULT 0,
	sort_order INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS invoice_photos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	invoice_id INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
	object_key TEXT NOT NULL,
	filename TEXT NOT NULL,
	content_type TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	uploader_username TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'user',
	signature TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plaid_items (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id TEXT NOT NULL UNIQUE,
	access_token TEXT NOT NULL,
	institution_name TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS service_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	phone TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	service_type TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'new',
	assigned_username TEXT NOT NULL DEFAULT '',
	progress_now TEXT NOT NULL DEFAULT '',
	progress_done TEXT NOT NULL DEFAULT '',
	progress_next TEXT NOT NULL DEFAULT '',
	expected_date TEXT NOT NULL DEFAULT '',
	tracking_code TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
`

func initDB() error {
	var err error
	db, err = sql.Open("sqlite", dbFile)
	if err != nil {
		return err
	}
	// SQLite works best with a single writer connection in this app.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		return err
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 10000;"); err != nil {
		return err
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		return err
	}
	// Enforce ON DELETE CASCADE behavior.
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		return err
	}
	_, err = db.Exec(schema)
	if err != nil {
		return err
	}
	return migrateSchema()
}

// backupDatabase writes a consistent, single-file snapshot of the live
// database to destPath, safe to copy elsewhere even while the app is running
// (unlike copying the .db/.db-wal/.db-shm files directly).
func backupDatabase(destPath string) error {
	_, err := db.Exec("VACUUM INTO ?", destPath)
	return err
}

// migrateSchema adds columns introduced after a database was first created,
// since "CREATE TABLE IF NOT EXISTS" doesn't alter existing tables.
func migrateSchema() error {
	rows, err := db.Query(`PRAGMA table_info(invoices)`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	columns := []string{"technician_username", "technician_signature", "customer_signature"}
	for _, col := range columns {
		if existing[col] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE invoices ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, col)); err != nil {
			return err
		}
	}
	if !existing["payment_status"] {
		if _, err := db.Exec(`ALTER TABLE invoices ADD COLUMN payment_status TEXT NOT NULL DEFAULT 'unpaid'`); err != nil {
			return err
		}
		if existing["paid"] {
			if _, err := db.Exec(`UPDATE invoices SET payment_status = 'paid' WHERE paid = 1`); err != nil {
				return err
			}
		}
	}
	if !existing["amount_paid"] {
		if _, err := db.Exec(`ALTER TABLE invoices ADD COLUMN amount_paid REAL NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		// Invoices already marked paid before this column existed: assume paid in full.
		if _, err := db.Exec(`UPDATE invoices SET amount_paid = total WHERE payment_status = 'paid'`); err != nil {
			return err
		}
	}

	urows, err := db.Query(`PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	userCols := map[string]bool{}
	for urows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := urows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			urows.Close()
			return err
		}
		userCols[name] = true
	}
	if err := urows.Err(); err != nil {
		return err
	}
	urows.Close()
	for _, col := range []string{"signature", "first_name", "last_name"} {
		if userCols[col] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE users ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, col)); err != nil {
			return err
		}
	}

	prows, err := db.Query(`PRAGMA table_info(invoice_photos)`)
	if err != nil {
		return err
	}
	photoCols := map[string]bool{}
	for prows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := prows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			prows.Close()
			return err
		}
		photoCols[name] = true
	}
	if err := prows.Err(); err != nil {
		return err
	}
	prows.Close()
	if !photoCols["uploader_username"] {
		if _, err := db.Exec(`ALTER TABLE invoice_photos ADD COLUMN uploader_username TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}

	srows, err := db.Query(`PRAGMA table_info(service_requests)`)
	if err != nil {
		return err
	}
	serviceCols := map[string]bool{}
	for srows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := srows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			srows.Close()
			return err
		}
		serviceCols[name] = true
	}
	if err := srows.Err(); err != nil {
		return err
	}
	srows.Close()
	if !serviceCols["assigned_username"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN assigned_username TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !serviceCols["progress_now"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN progress_now TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !serviceCols["progress_done"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN progress_done TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !serviceCols["progress_next"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN progress_next TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !serviceCols["expected_date"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN expected_date TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !serviceCols["tracking_code"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN tracking_code TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !serviceCols["updated_at"] {
		if _, err := db.Exec(`ALTER TABLE service_requests ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if err := backfillServiceRequestTrackingCodes(); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_service_requests_tracking_code ON service_requests(tracking_code)`); err != nil {
		return err
	}
	return nil
}

var nonDigits = regexp.MustCompile(`\D+`)

func normalizePhone(phone string) string {
	return nonDigits.ReplaceAllString(phone, "")
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Client is a customer contact record.
type Client struct {
	ID    int64
	Name  string
	Phone string
	Email string
}

type ServiceRequest struct {
	ID          int64
	Name        string
	Phone       string
	Email       string
	ServiceType string
	Message     string
	Status      string
	AssignedTo  string
	ProgressNow string
	ProgressDone string
	ProgressNext string
	ExpectedDate string
	TrackingCode string
	UpdatedAt   string
	CreatedAt   string
}

func generateTrackingCode() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	code = strings.TrimSpace(code)
	if len(code) > 10 {
		code = code[:10]
	}
	return "SR-" + code, nil
}

func backfillServiceRequestTrackingCodes() error {
	rows, err := db.Query(`SELECT id FROM service_requests WHERE tracking_code = ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		code, err := generateTrackingCode()
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE service_requests SET tracking_code = ?, updated_at = COALESCE(NULLIF(updated_at, ''), created_at) WHERE id = ?`, code, id); err != nil {
			return err
		}
	}
	return nil
}

func createServiceRequest(name, phone, email, serviceType, message string) (int64, string, error) {
	code := ""
	for i := 0; i < 5; i++ {
		candidate, err := generateTrackingCode()
		if err != nil {
			return 0, "", err
		}
		var exists int
		err = db.QueryRow(`SELECT COUNT(*) FROM service_requests WHERE tracking_code = ?`, candidate).Scan(&exists)
		if err != nil {
			return 0, "", err
		}
		if exists == 0 {
			code = candidate
			break
		}
	}
	if code == "" {
		return 0, "", fmt.Errorf("could not generate unique tracking code")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(
		`INSERT INTO service_requests (name, phone, email, service_type, message, status, tracking_code, updated_at, created_at) VALUES (?, ?, ?, ?, ?, 'new', ?, ?, ?)`,
		strings.TrimSpace(name),
		normalizePhone(phone),
		normalizeEmail(email),
		strings.TrimSpace(serviceType),
		strings.TrimSpace(message),
		code,
		now,
		now,
	)
	if err != nil {
		return 0, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	return id, code, nil
}

func listServiceRequests(limit int) ([]ServiceRequest, error) {
	return listServiceRequestsWhere("", limit)
}

func listAssignedServiceRequests(username string, limit int) ([]ServiceRequest, error) {
	return listServiceRequestsWhere(strings.TrimSpace(username), limit)
}

func listServiceRequestsWhere(assignedUsername string, limit int) ([]ServiceRequest, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, name, phone, email, service_type, message, status,
		        assigned_username, progress_now, progress_done, progress_next,
		        expected_date, tracking_code, updated_at, created_at
		 FROM service_requests`
	args := []any{}
	if assignedUsername != "" {
		query += " WHERE assigned_username = ?"
		args = append(args, assignedUsername)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ServiceRequest
	for rows.Next() {
		var r ServiceRequest
		if err := rows.Scan(
			&r.ID,
			&r.Name,
			&r.Phone,
			&r.Email,
			&r.ServiceType,
			&r.Message,
			&r.Status,
			&r.AssignedTo,
			&r.ProgressNow,
			&r.ProgressDone,
			&r.ProgressNext,
			&r.ExpectedDate,
			&r.TrackingCode,
			&r.UpdatedAt,
			&r.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func getServiceRequestByTrackingCode(code string) (ServiceRequest, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	var r ServiceRequest
	err := db.QueryRow(
		`SELECT id, name, phone, email, service_type, message, status,
		        assigned_username, progress_now, progress_done, progress_next,
		        expected_date, tracking_code, updated_at, created_at
		 FROM service_requests WHERE tracking_code = ?`,
		code,
	).Scan(
		&r.ID,
		&r.Name,
		&r.Phone,
		&r.Email,
		&r.ServiceType,
		&r.Message,
		&r.Status,
		&r.AssignedTo,
		&r.ProgressNow,
		&r.ProgressDone,
		&r.ProgressNext,
		&r.ExpectedDate,
		&r.TrackingCode,
		&r.UpdatedAt,
		&r.CreatedAt,
	)
	return r, err
}

func updateServiceRequestWorkflow(id int64, assignedTo, status, progressNow, progressDone, progressNext, expectedDate string) error {
	_, err := db.Exec(
		`UPDATE service_requests
		 SET assigned_username = ?, status = ?, progress_now = ?, progress_done = ?, progress_next = ?, expected_date = ?, updated_at = ?
		 WHERE id = ?`,
		strings.TrimSpace(assignedTo),
		strings.TrimSpace(status),
		strings.TrimSpace(progressNow),
		strings.TrimSpace(progressDone),
		strings.TrimSpace(progressNext),
		strings.TrimSpace(expectedDate),
		time.Now().UTC().Format(time.RFC3339),
		id,
	)
	return err
}

// findOrCreateClient matches an existing client by phone, then email, then
// case-insensitive name, filling in any missing contact details on match.
// A new client is created if none of those match.
func findOrCreateClient(name, phone, email string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Unnamed Client"
	}
	normPhone := normalizePhone(phone)
	normEmail := normalizeEmail(email)

	var id int64
	var existingPhone, existingEmail string
	var err error

	if normPhone != "" {
		err = db.QueryRow(`SELECT id, phone, email FROM clients WHERE phone <> '' AND phone = ?`, normPhone).
			Scan(&id, &existingPhone, &existingEmail)
	}
	if (err == nil && id == 0) || errors.Is(err, sql.ErrNoRows) || normPhone == "" {
		if normEmail != "" {
			err = db.QueryRow(`SELECT id, phone, email FROM clients WHERE email <> '' AND email = ?`, normEmail).
				Scan(&id, &existingPhone, &existingEmail)
		}
	}
	if errors.Is(err, sql.ErrNoRows) || (normPhone == "" && normEmail == "") {
		err = db.QueryRow(`SELECT id, phone, email FROM clients WHERE name = ? COLLATE NOCASE`, name).
			Scan(&id, &existingPhone, &existingEmail)
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	if id != 0 {
		// Backfill any contact details that were missing before.
		newPhone := existingPhone
		if newPhone == "" {
			newPhone = normPhone
		}
		newEmail := existingEmail
		if newEmail == "" {
			newEmail = normEmail
		}
		if newPhone != existingPhone || newEmail != existingEmail {
			if _, err := db.Exec(`UPDATE clients SET phone = ?, email = ? WHERE id = ?`, newPhone, newEmail, id); err != nil {
				return 0, err
			}
		}
		return id, nil
	}

	res, err := db.Exec(
		`INSERT INTO clients (name, phone, email, created_at) VALUES (?, ?, ?, ?)`,
		name, normPhone, normEmail, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// saveInvoiceRecord inserts a new invoice (invoiceID == 0) or updates an
// existing one, replacing its job line items.
func saveInvoiceRecord(invoiceID int64, clientID int64, draft InvoiceData) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	total := totalOf(draft.Jobs)

	if invoiceID == 0 {
		res, err := tx.Exec(
			`INSERT INTO invoices (client_id, invoice_date, location, time_in, time_out, total, technician_username, technician_signature, customer_signature, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			clientID, draft.InvoiceDate, draft.Location, draft.TimeIn, draft.TimeOut, total,
			draft.TechnicianUsername, draft.TechnicianSignature, draft.CustomerSignature, now, now,
		)
		if err != nil {
			return 0, err
		}
		invoiceID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.Exec(
			`UPDATE invoices SET client_id = ?, invoice_date = ?, location = ?, time_in = ?, time_out = ?, total = ?,
			 technician_username = ?, technician_signature = ?, customer_signature = ?, updated_at = ?
			 WHERE id = ?`,
			clientID, draft.InvoiceDate, draft.Location, draft.TimeIn, draft.TimeOut, total,
			draft.TechnicianUsername, draft.TechnicianSignature, draft.CustomerSignature, now, invoiceID,
		); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM invoice_jobs WHERE invoice_id = ?`, invoiceID); err != nil {
			return 0, err
		}
	}

	for i, j := range draft.Jobs {
		if _, err := tx.Exec(
			`INSERT INTO invoice_jobs (invoice_id, description, price, sort_order) VALUES (?, ?, ?, ?)`,
			invoiceID, j.Description, j.Price, i,
		); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return invoiceID, nil
}

// ClientSummary is a row in the clients list page.
type ClientSummary struct {
	ID            int64
	Name          string
	Phone         string
	Email         string
	InvoiceCount  int
	LifetimeTotal float64
	UnpaidCount   int
	OwedTotal     float64
}

func listClients(search string) ([]ClientSummary, error) {
	query := `
		SELECT c.id, c.name, c.phone, c.email,
		       COUNT(i.id) AS invoice_count,
		       COALESCE(SUM(i.total), 0) AS lifetime_total,
		       SUM(CASE WHEN i.payment_status IN ('unpaid', 'partial') THEN 1 ELSE 0 END) AS unpaid_count,
		       COALESCE(SUM(i.total - i.amount_paid), 0) AS owed_total
		FROM clients c
		LEFT JOIN invoices i ON i.client_id = c.id
	`
	args := []any{}
	if search = strings.TrimSpace(search); search != "" {
		query += ` WHERE c.name LIKE ? OR c.phone LIKE ? OR c.email LIKE ? `
		like := "%" + search + "%"
		args = append(args, like, like, like)
	}
	query += ` GROUP BY c.id ORDER BY c.name COLLATE NOCASE ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClientSummary
	for rows.Next() {
		var c ClientSummary
		if err := rows.Scan(&c.ID, &c.Name, &c.Phone, &c.Email, &c.InvoiceCount, &c.LifetimeTotal, &c.UnpaidCount, &c.OwedTotal); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func getClientTotalDebt(id int64) (float64, error) {
	var debt float64
	err := db.QueryRow(`SELECT COALESCE(SUM(total - amount_paid), 0) FROM invoices WHERE client_id = ?`, id).Scan(&debt)
	return debt, err
}

func getClient(id int64) (Client, error) {
	var c Client
	err := db.QueryRow(`SELECT id, name, phone, email FROM clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.Phone, &c.Email)
	return c, err
}

func getClientLifetimeTotal(id int64) (float64, error) {
	var total float64
	err := db.QueryRow(`SELECT COALESCE(SUM(total), 0) FROM invoices WHERE client_id = ?`, id).Scan(&total)
	return total, err
}

// InvoiceSummary is a row in a client's invoice history table.
type InvoiceSummary struct {
	ID            int64
	InvoiceDate   string
	Total         float64
	JobCount      int
	PaymentStatus string
	AmountPaid    float64
	Debt          float64
}

func getClientInvoices(clientID int64, statusFilter string) ([]InvoiceSummary, error) {
	query := `
		SELECT i.id, i.invoice_date, i.total, i.payment_status, i.amount_paid, COUNT(j.id) AS job_count
		FROM invoices i
		LEFT JOIN invoice_jobs j ON j.invoice_id = i.id
		WHERE i.client_id = ?
	`
	args := []any{clientID}
	if statusFilter != "" {
		query += ` AND i.payment_status = ? `
		args = append(args, statusFilter)
	}
	query += ` GROUP BY i.id ORDER BY i.invoice_date DESC, i.id DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InvoiceSummary
	for rows.Next() {
		var s InvoiceSummary
		if err := rows.Scan(&s.ID, &s.InvoiceDate, &s.Total, &s.PaymentStatus, &s.AmountPaid, &s.JobCount); err != nil {
			return nil, err
		}
		s.Debt = s.Total - s.AmountPaid
		out = append(out, s)
	}
	return out, rows.Err()
}

// setInvoicePaymentStatus updates an invoice's payment status ("unpaid", "partial", or "paid")
// along with the amount paid so far (ignored/clamped for "unpaid"/"paid").
func setInvoicePaymentStatus(id int64, status string, amountPaid float64) error {
	var total float64
	if err := db.QueryRow(`SELECT total FROM invoices WHERE id = ?`, id).Scan(&total); err != nil {
		return err
	}
	switch status {
	case "paid":
		amountPaid = total
	case "unpaid":
		amountPaid = 0
	default: // partial
		if amountPaid < 0 {
			amountPaid = 0
		}
		if amountPaid > total {
			amountPaid = total
		}
	}
	_, err := db.Exec(`UPDATE invoices SET payment_status = ?, amount_paid = ? WHERE id = ?`, status, amountPaid, id)
	return err
}

// addInvoicePayment records an additional installment payment on top of what
// was already paid, clamping to the invoice total, and recalculates the
// payment status ("paid" once fully covered, otherwise "partial"/"unpaid").
func addInvoicePayment(id int64, amount float64) error {
	var total, amountPaid float64
	if err := db.QueryRow(`SELECT total, amount_paid FROM invoices WHERE id = ?`, id).Scan(&total, &amountPaid); err != nil {
		return err
	}
	amountPaid += amount
	if amountPaid < 0 {
		amountPaid = 0
	}
	if amountPaid > total {
		amountPaid = total
	}
	status := "partial"
	if amountPaid <= 0 {
		status = "unpaid"
	} else if amountPaid >= total {
		status = "paid"
	}
	_, err := db.Exec(`UPDATE invoices SET payment_status = ?, amount_paid = ? WHERE id = ?`, status, amountPaid, id)
	return err
}

// getInvoiceWithJobs reloads a historical invoice along with its client.
func getInvoiceWithJobs(id int64) (InvoiceData, Client, error) {
	var inv InvoiceData
	var client Client
	err := db.QueryRow(`
		SELECT i.client_id, i.invoice_date, i.location, i.time_in, i.time_out,
		       i.technician_username, i.technician_signature, i.customer_signature, i.payment_status, i.amount_paid,
		       c.name, c.phone, c.email
		FROM invoices i
		JOIN clients c ON c.id = i.client_id
		WHERE i.id = ?
	`, id).Scan(&client.ID, &inv.InvoiceDate, &inv.Location, &inv.TimeIn, &inv.TimeOut,
		&inv.TechnicianUsername, &inv.TechnicianSignature, &inv.CustomerSignature, &inv.PaymentStatus, &inv.AmountPaid,
		&client.Name, &client.Phone, &client.Email)
	if err != nil {
		return inv, client, err
	}
	inv.ClientName = client.Name
	inv.Phone = client.Phone
	inv.Email = client.Email

	rows, err := db.Query(`SELECT id, description, price FROM invoice_jobs WHERE invoice_id = ? ORDER BY sort_order ASC, id ASC`, id)
	if err != nil {
		return inv, client, err
	}
	defer rows.Close()

	nextID := 1
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Description, &j.Price); err != nil {
			return inv, client, err
		}
		inv.Jobs = append(inv.Jobs, j)
		if j.ID >= nextID {
			nextID = j.ID + 1
		}
	}
	if err := rows.Err(); err != nil {
		return inv, client, err
	}
	inv.NextJobID = nextID
	return inv, client, nil
}

// InvoicePhoto stores metadata for evidence photos attached to an invoice.
type InvoicePhoto struct {
	ID          int64
	InvoiceID   int64
	ObjectKey   string
	Filename    string
	ContentType string
	SizeBytes   int64
	Uploader    string
	CreatedAt   string
}

func createInvoicePhoto(invoiceID int64, objectKey, filename, contentType, uploader string, sizeBytes int64) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO invoice_photos (invoice_id, object_key, filename, content_type, size_bytes, uploader_username, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		invoiceID, objectKey, filename, contentType, sizeBytes, uploader, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func listInvoicePhotos(invoiceID int64) ([]InvoicePhoto, error) {
	rows, err := db.Query(
		`SELECT id, invoice_id, object_key, filename, content_type, size_bytes, uploader_username, created_at
		 FROM invoice_photos WHERE invoice_id = ? ORDER BY id DESC`,
		invoiceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var photos []InvoicePhoto
	for rows.Next() {
		var p InvoicePhoto
		if err := rows.Scan(&p.ID, &p.InvoiceID, &p.ObjectKey, &p.Filename, &p.ContentType, &p.SizeBytes, &p.Uploader, &p.CreatedAt); err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, rows.Err()
}

func getInvoicePhoto(photoID int64) (InvoicePhoto, error) {
	var p InvoicePhoto
	err := db.QueryRow(
		`SELECT id, invoice_id, object_key, filename, content_type, size_bytes, uploader_username, created_at
		 FROM invoice_photos WHERE id = ?`,
		photoID,
	).Scan(&p.ID, &p.InvoiceID, &p.ObjectKey, &p.Filename, &p.ContentType, &p.SizeBytes, &p.Uploader, &p.CreatedAt)
	return p, err
}

func deleteInvoicePhoto(photoID int64) error {
	_, err := db.Exec(`DELETE FROM invoice_photos WHERE id = ?`, photoID)
	return err
}

func invoiceExists(id int64) (bool, error) {
	var exists int
	err := db.QueryRow(`SELECT 1 FROM invoices WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func clientTitle(name, invoiceDate string) string {
	return fmt.Sprintf("%s_%s", slugify(name), invoiceDate)
}

// deleteInvoiceRecord removes an invoice and its jobs (via ON DELETE CASCADE).
func deleteInvoiceRecord(id int64) error {
	res, err := db.Exec(`DELETE FROM invoices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// User is a login account. Role is either "admin" or "user".
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Signature    string // data:image/png;base64,... saved once from the account page
	FirstName    string
	LastName     string
}

// FullName returns "First Last", falling back to the username if no name is set.
func (u User) FullName() string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		return u.Username
	}
	return name
}

// seedDefaultAdmin creates the built-in admin/admin account the first time
// the app runs, so there's always a way to log in without any manual setup.
// It's a no-op once at least one user exists.
func seedDefaultAdmin() error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return createUser("admin", "admin", "admin", "", "")
}

func createUser(username, plainPassword, role, firstName, lastName string) error {
	hash, err := bcryptHash(plainPassword)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT INTO users (username, password_hash, role, created_at, first_name, last_name) VALUES (?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(username), hash, role, time.Now().Format(time.RFC3339), strings.TrimSpace(firstName), strings.TrimSpace(lastName),
	)
	return err
}

func getUserByUsername(username string) (User, error) {
	var u User
	err := db.QueryRow(
		`SELECT id, username, password_hash, role, signature, first_name, last_name FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Signature, &u.FirstName, &u.LastName)
	return u, err
}

func getUserByID(id int64) (User, error) {
	var u User
	err := db.QueryRow(
		`SELECT id, username, password_hash, role, signature, first_name, last_name FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Signature, &u.FirstName, &u.LastName)
	return u, err
}

// setUserSignature saves the account-level signature image used on invoices
// this user finalizes.
func setUserSignature(username, signature string) error {
	_, err := db.Exec(`UPDATE users SET signature = ? WHERE username = ?`, signature, username)
	return err
}

func listUsers() ([]User, error) {
	rows, err := db.Query(`SELECT id, username, password_hash, role, signature, first_name, last_name FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Signature, &u.FirstName, &u.LastName); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func countAdmins() (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&count)
	return count, err
}

func updateUserPassword(id int64, plainPassword string) error {
	hash, err := bcryptHash(plainPassword)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// updateUserRole promotes/demotes a user between "admin" and "user".
func updateUserRole(id int64, role string) error {
	_, err := db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

// updateUserName sets a user's first/last name shown in the nav bar.
func updateUserName(id int64, firstName, lastName string) error {
	_, err := db.Exec(`UPDATE users SET first_name = ?, last_name = ? WHERE id = ?`, strings.TrimSpace(firstName), strings.TrimSpace(lastName), id)
	return err
}

func deleteUser(id int64) error {
	_, err := db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// PlaidItem is a bank connection (Plaid "Item") created via Plaid Link.
type PlaidItem struct {
	ID              int64
	ItemID          string
	AccessToken     string
	InstitutionName string
	CreatedAt       string
}

// savePlaidItem persists a newly-linked bank account's access token.
func savePlaidItem(itemID, accessToken, institutionName string) error {
	_, err := db.Exec(
		`INSERT INTO plaid_items (item_id, access_token, institution_name, created_at) VALUES (?, ?, ?, ?)`,
		itemID, accessToken, institutionName, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// listPlaidItems returns all connected bank accounts, most recent first.
func listPlaidItems() ([]PlaidItem, error) {
	rows, err := db.Query(`SELECT id, item_id, access_token, institution_name, created_at FROM plaid_items ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PlaidItem
	for rows.Next() {
		var p PlaidItem
		if err := rows.Scan(&p.ID, &p.ItemID, &p.AccessToken, &p.InstitutionName, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// deletePlaidItem removes a stored bank connection.
func deletePlaidItem(id int64) error {
	_, err := db.Exec(`DELETE FROM plaid_items WHERE id = ?`, id)
	return err
}

