// Outbound email delivery (invoice PDF attachments) via SMTP.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"net/smtp"
	"net/url"
	"os"
	"strings"
)

type smtpConfig struct {
	host, port, username, password, from string
}

type emailAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
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
	attachments := []emailAttachment{{
		Filename:    filename,
		ContentType: "application/pdf",
		Data:        pdfBytes,
	}}
	return sendInvoiceEmailWithAttachments(to, cc, subject, body, attachments)
}

func sendInvoiceEmailWithAttachments(to, cc, subject, body string, attachments []emailAttachment) error {
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

	for _, a := range attachments {
		if len(a.Data) == 0 || strings.TrimSpace(a.Filename) == "" {
			continue
		}
		ctype := strings.TrimSpace(a.ContentType)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		fmt.Fprintf(&msg, "--%s\r\n", boundary)
		fmt.Fprintf(&msg, "Content-Type: %s\r\n", ctype)
		fmt.Fprintf(&msg, "Content-Transfer-Encoding: base64\r\n")
		fmt.Fprintf(&msg, "Content-Disposition: attachment; filename=%q\r\n\r\n", a.Filename)
		encoded := base64.StdEncoding.EncodeToString(a.Data)
		for i := 0; i < len(encoded); i += 76 {
			end := i + 76
			if end > len(encoded) {
				end = len(encoded)
			}
			msg.WriteString(encoded[i:end])
			msg.WriteString("\r\n")
		}
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

func sendServiceRequestConfirmation(to, name, serviceType, trackingCode string) error {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_SITE_URL")), "/")
	if baseURL == "" {
		baseURL = "https://www.electroclimapro.us"
	}
	service := strings.TrimSpace(serviceType)
	if service == "" {
		service = "servicio general"
	}
	body := fmt.Sprintf(`Hola %s,

Gracias por contactar a Electroclima Pro Services. Recibimos correctamente tu solicitud de %s.

Nuestro equipo revisara tu solicitud y se comunicara contigo lo antes posible para confirmar los proximos pasos.

Tu codigo de seguimiento es: %s
Consulta el estado de tu servicio aqui: %s/track?code=%s

Guarda este codigo para revisar el progreso del trabajo. Te mostrara lo que estamos haciendo, lo que terminamos y el proximo paso.

Saludos,
Electroclima Pro Services, LLC
786 389 3330
electroclimaprollc@gmail.com`,
		name,
		service,
		trackingCode,
		baseURL,
		url.QueryEscape(trackingCode),
	)
	return sendInvoiceEmailWithAttachments(
		to,
		"",
		"Solicitud de servicio recibida - Codigo "+trackingCode,
		body,
		nil,
	)
}
