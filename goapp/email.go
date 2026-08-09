// Outbound email delivery (invoice PDF attachments) via SMTP.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"net/smtp"
	"os"
	"strings"
)

type smtpConfig struct {
	host, port, username, password, from string
}

func loadSMTPConfig() (smtpConfig, error) {
	cfg := smtpConfig{
		host:     os.Getenv("SMTP_HOST"),
		port:     os.Getenv("SMTP_PORT"),
		username: os.Getenv("SMTP_USERNAME"),
		password: os.Getenv("SMTP_PASSWORD"),
		from:     os.Getenv("SMTP_FROM"),
	}
	if cfg.host == "" || cfg.port == "" || cfg.username == "" || cfg.password == "" {
		return cfg, fmt.Errorf("email is not configured: set SMTP_HOST, SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD in runtime environment")
	}
	if cfg.from == "" {
		cfg.from = cfg.username
	}
	return cfg, nil
}

// sendInvoiceEmail emails the given PDF as an attachment to "to" (optionally
// CC'ing "cc") using the SMTP settings from the environment.
func sendInvoiceEmail(to, cc, subject, body string, pdfBytes []byte, filename string) error {
	cfg, err := loadSMTPConfig()
	if err != nil {
		return err
	}

	boundary := "invoiceapp-boundary-42"
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: Electroclima Pro Services <%s>\r\n", cfg.from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	if cc != "" {
		fmt.Fprintf(&msg, "Cc: %s\r\n", cc)
	}
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	fmt.Fprintf(&msg, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&msg, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)

	fmt.Fprintf(&msg, "--%s\r\n", boundary)
	fmt.Fprintf(&msg, "Content-Type: text/plain; charset=\"UTF-8\"\r\n\r\n")
	msg.WriteString(body)
	msg.WriteString("\r\n\r\n")

	fmt.Fprintf(&msg, "--%s\r\n", boundary)
	fmt.Fprintf(&msg, "Content-Type: application/pdf\r\n")
	fmt.Fprintf(&msg, "Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(&msg, "Content-Disposition: attachment; filename=%q\r\n\r\n", filename)
	encoded := base64.StdEncoding.EncodeToString(pdfBytes)
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		msg.WriteString(encoded[i:end])
		msg.WriteString("\r\n")
	}
	fmt.Fprintf(&msg, "--%s--\r\n", boundary)

	auth := smtp.PlainAuth("", cfg.username, cfg.password, cfg.host)
	addr := cfg.host + ":" + cfg.port
	toList := []string{strings.TrimSpace(to)}
	if cc != "" {
		toList = append(toList, strings.TrimSpace(cc))
	}
	return smtp.SendMail(addr, auth, cfg.from, toList, msg.Bytes())
}
