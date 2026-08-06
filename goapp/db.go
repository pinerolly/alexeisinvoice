// Database access for client and invoice history, backed by SQLite.
package main

import (
	"database/sql"
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
`

func initDB() error {
	var err error
	db, err = sql.Open("sqlite", dbFile)
	if err != nil {
		return err
	}
	if err := db.Ping(); err != nil {
		return err
	}
	// Enforce ON DELETE CASCADE behavior.
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		return err
	}
	_, err = db.Exec(schema)
	return err
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
			`INSERT INTO invoices (client_id, invoice_date, location, time_in, time_out, total, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			clientID, draft.InvoiceDate, draft.Location, draft.TimeIn, draft.TimeOut, total, now, now,
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
			`UPDATE invoices SET client_id = ?, invoice_date = ?, location = ?, time_in = ?, time_out = ?, total = ?, updated_at = ?
			 WHERE id = ?`,
			clientID, draft.InvoiceDate, draft.Location, draft.TimeIn, draft.TimeOut, total, now, invoiceID,
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
	ID           int64
	Name         string
	Phone        string
	Email        string
	InvoiceCount int
	LifetimeTotal float64
}

func listClients(search string) ([]ClientSummary, error) {
	query := `
		SELECT c.id, c.name, c.phone, c.email,
		       COUNT(i.id) AS invoice_count,
		       COALESCE(SUM(i.total), 0) AS lifetime_total
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
		if err := rows.Scan(&c.ID, &c.Name, &c.Phone, &c.Email, &c.InvoiceCount, &c.LifetimeTotal); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func getClient(id int64) (Client, error) {
	var c Client
	err := db.QueryRow(`SELECT id, name, phone, email FROM clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.Phone, &c.Email)
	return c, err
}

// InvoiceSummary is a row in a client's invoice history table.
type InvoiceSummary struct {
	ID          int64
	InvoiceDate string
	Total       float64
	JobCount    int
}

func getClientInvoices(clientID int64) ([]InvoiceSummary, error) {
	rows, err := db.Query(`
		SELECT i.id, i.invoice_date, i.total, COUNT(j.id) AS job_count
		FROM invoices i
		LEFT JOIN invoice_jobs j ON j.invoice_id = i.id
		WHERE i.client_id = ?
		GROUP BY i.id
		ORDER BY i.invoice_date DESC, i.id DESC
	`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InvoiceSummary
	for rows.Next() {
		var s InvoiceSummary
		if err := rows.Scan(&s.ID, &s.InvoiceDate, &s.Total, &s.JobCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// getInvoiceWithJobs reloads a historical invoice along with its client.
func getInvoiceWithJobs(id int64) (InvoiceData, Client, error) {
	var inv InvoiceData
	var client Client
	err := db.QueryRow(`
		SELECT i.client_id, i.invoice_date, i.location, i.time_in, i.time_out,
		       c.name, c.phone, c.email
		FROM invoices i
		JOIN clients c ON c.id = i.client_id
		WHERE i.id = ?
	`, id).Scan(&client.ID, &inv.InvoiceDate, &inv.Location, &inv.TimeIn, &inv.TimeOut,
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
