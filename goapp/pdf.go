// PDF rendering for invoices, used both by the download link and by email.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
)

const (
	pdfPrimaryR, pdfPrimaryG, pdfPrimaryB = 28, 61, 90    // #1c3d5a
	pdfAccentR, pdfAccentG, pdfAccentB    = 46, 134, 171  // #2e86ab
	pdfLightR, pdfLightG, pdfLightB       = 244, 247, 249 // #f4f7f9
)

// generateInvoicePDF renders an invoice as a PDF document matching the
// on-screen layout, returning the raw PDF bytes. invoiceID is the permanent,
// never-reused invoice number (the DB row's autoincrement id).
func generateInvoicePDF(inv InvoiceData, client Client, title string, _ int64) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetTitle(title, false)
	pdf.SetMargins(18, 18, 18)
	pdf.AddPage()

	pageW, _ := pdf.GetPageSize()
	contentW := pageW - 36

	// Header: company name/details (left) and INVOICE title/date (right).
	pdf.SetFont("Arial", "B", 16)
	pdf.SetTextColor(pdfPrimaryR, pdfPrimaryG, pdfPrimaryB)
	pdf.CellFormat(contentW*0.6, 8, "ELECTROCLIMA PRO SERVICES, LLC", "", 0, "L", false, 0, "")
	pdf.SetFont("Arial", "B", 22)
	pdf.SetTextColor(pdfAccentR, pdfAccentG, pdfAccentB)
	pdf.CellFormat(contentW*0.4, 8, "INVOICE", "", 1, "R", false, 0, "")

	pdf.SetFont("Arial", "", 10)
	pdf.SetTextColor(80, 80, 80)
	pdf.CellFormat(contentW*0.6, 5, "10530 NW 35 CT, Miami, FL 33147", "", 0, "L", false, 0, "")
	pdf.CellFormat(contentW, 5, "Tel: 786 389 3330  |  Email: electroclimaprollc@gmail.com", "", 1, "L", false, 0, "")

	pdf.SetDrawColor(pdfPrimaryR, pdfPrimaryG, pdfPrimaryB)
	pdf.SetLineWidth(0.8)
	y := pdf.GetY() + 3
	pdf.Line(18, y, pageW-18, y)
	pdf.SetY(y + 6)

	// Client / Service Details boxes.
	boxW := contentW/2 - 3
	boxTop := pdf.GetY()
	drawInfoBox(pdf, 18, boxTop, boxW, "CLIENT", clientLines(inv))
	drawInfoBox(pdf, 18+boxW+6, boxTop, boxW, "SERVICE DETAILS", []string{
		"Time In: " + formatTimeDisplayOrDash(inv.TimeIn),
		"Time Out: " + formatTimeDisplayOrDash(inv.TimeOut),
	})
	pdf.SetY(boxTop + 32)

	// Job table.
	pdf.SetFont("Arial", "B", 10)
	pdf.SetFillColor(pdfPrimaryR, pdfPrimaryG, pdfPrimaryB)
	pdf.SetTextColor(255, 255, 255)
	descW := contentW * 0.78
	priceW := contentW * 0.22
	pdf.CellFormat(descW, 9, "  DESCRIPTION OF WORK PERFORMED", "", 0, "L", true, 0, "")
	pdf.CellFormat(priceW, 9, "PRICE  ", "", 1, "R", true, 0, "")

	pdf.SetFont("Arial", "", 10)
	pdf.SetTextColor(40, 40, 40)
	fill := false
	for _, j := range inv.Jobs {
		desc := j.Description
		if desc == "" {
			desc = " "
		}
		lines := pdf.SplitLines([]byte(desc), descW-4)
		rowH := float64(len(lines)) * 5.5
		if rowH < 9 {
			rowH = 9
		}
		if fill {
			pdf.SetFillColor(pdfLightR, pdfLightG, pdfLightB)
		} else {
			pdf.SetFillColor(255, 255, 255)
		}
		x, rowY := pdf.GetXY()
		pdf.Rect(x, rowY, contentW, rowH, "F")
		pdf.SetXY(x+2, rowY+1.5)
		pdf.MultiCell(descW-4, 5.5, desc, "", "L", false)
		pdf.SetXY(x+descW, rowY+1.5)
		pdf.CellFormat(priceW-2, rowH-1.5, money(j.Price), "", 0, "R", false, 0, "")
		pdf.SetXY(x, rowY+rowH)
		pdf.SetDrawColor(217, 226, 232)
		pdf.Line(x, rowY+rowH, x+contentW, rowY+rowH)
		fill = !fill
	}

	// Total.
	pdf.Ln(6)
	pdf.SetFont("Arial", "B", 12)
	pdf.SetTextColor(pdfPrimaryR, pdfPrimaryG, pdfPrimaryB)
	pdf.SetDrawColor(pdfPrimaryR, pdfPrimaryG, pdfPrimaryB)
	totalY := pdf.GetY()
	pdf.Line(18+contentW-70, totalY, pageW-18, totalY)
	pdf.SetY(totalY + 2)
	pdf.CellFormat(contentW-40, 8, "Total Amount Due", "", 0, "R", false, 0, "")
	pdf.CellFormat(40, 8, money(totalOf(inv.Jobs)), "", 1, "R", false, 0, "")

	// Signatures.
	pdf.Ln(10)
	sigW := contentW/2 - 5
	const sigImgMaxH = 16.0
	imgTop := pdf.GetY()
	drawSignatureImage(pdf, "sig-tech", inv.TechnicianSignature, 18, imgTop, sigW, sigImgMaxH)
	drawSignatureImage(pdf, "sig-cust", inv.CustomerSignature, 18+sigW+10, imgTop, sigW, sigImgMaxH)
	sigY := imgTop + sigImgMaxH + 2
	pdf.SetDrawColor(150, 150, 150)
	pdf.Line(18, sigY, 18+sigW, sigY)
	pdf.Line(18+sigW+10, sigY, 18+sigW+10+sigW, sigY)
	pdf.SetFont("Arial", "", 9)
	pdf.SetTextColor(100, 100, 100)
	techLabel := "TECHNICIAN SIGNATURE"
	if inv.TechnicianUsername != "" {
		techLabel += " (" + inv.TechnicianUsername + ")"
	}
	custLabel := "CUSTOMER SIGNATURE"
	if inv.ClientName != "" {
		custLabel += " (" + inv.ClientName + ")"
	}
	pdf.SetXY(18, sigY+1)
	pdf.CellFormat(sigW, 5, techLabel, "", 0, "L", false, 0, "")
	pdf.SetXY(18+sigW+10, sigY+1)
	pdf.CellFormat(sigW, 5, custLabel, "", 1, "L", false, 0, "")

	// Footer note.
	pdf.SetY(-25)
	pdf.SetFont("Arial", "", 9)
	pdf.SetTextColor(136, 136, 136)
	pdf.CellFormat(contentW, 5, "Thank you for your business!  |  Electroclima Pro Services, LLC  |  786 389 3330", "", 1, "C", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clientLines(inv InvoiceData) []string {
	lines := []string{inv.ClientName}
	if inv.Phone != "" {
		lines = append(lines, "Phone: "+inv.Phone)
	}
	if inv.Email != "" {
		lines = append(lines, "Email: "+inv.Email)
	}
	lines = append(lines, "Location: "+inv.Location)
	return lines
}

func formatTimeDisplayOrDash(t string) string {
	if t == "" {
		return "-"
	}
	return formatTimeDisplay(t)
}

// drawSignatureImage decodes a "data:image/png;base64,..." signature and
// places it left-aligned above (x, y), scaled to fit within maxW x maxH.
func drawSignatureImage(pdf *fpdf.Fpdf, name, dataURL string, x, y, maxW, maxH float64) {
	if !isValidPNGDataURL(dataURL) {
		return
	}
	idx := strings.Index(dataURL, ",")
	raw, err := base64.StdEncoding.DecodeString(dataURL[idx+1:])
	if err != nil {
		return
	}
	opts := fpdf.ImageOptions{ImageType: "png"}
	info := pdf.RegisterImageOptionsReader(name, opts, bytes.NewReader(raw))
	if info == nil {
		return
	}
	w, h := info.Extent()
	if w <= 0 || h <= 0 {
		return
	}
	scale := maxW / w
	if h*scale > maxH {
		scale = maxH / h
	}
	pdf.ImageOptions(name, x, y, w*scale, h*scale, false, opts, 0, "")
}

// isValidPNGDataURL reports whether s is a well-formed "data:image/png;base64,..."
// URL: the only signature format the app produces (via canvas.toDataURL) and
// accepts, so this doubles as validation before trusting it as a safe URL.
func isValidPNGDataURL(s string) bool {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(s[len(prefix):])
	return err == nil
}

func drawInfoBox(pdf *fpdf.Fpdf, x, y, w float64, heading string, lines []string) {
	h := 32.0
	pdf.SetFillColor(pdfLightR, pdfLightG, pdfLightB)
	pdf.Rect(x, y, w, h, "F")
	pdf.SetDrawColor(pdfAccentR, pdfAccentG, pdfAccentB)
	pdf.SetLineWidth(1)
	pdf.Line(x, y, x, y+h)

	pdf.SetXY(x+4, y+3)
	pdf.SetFont("Arial", "B", 9)
	pdf.SetTextColor(pdfPrimaryR, pdfPrimaryG, pdfPrimaryB)
	pdf.CellFormat(w-8, 5, heading, "", 2, "L", false, 0, "")

	pdf.SetFont("Arial", "", 10)
	pdf.SetTextColor(40, 40, 40)
	for _, line := range lines {
		pdf.SetX(x + 4)
		pdf.CellFormat(w-8, 5, line, "", 2, "L", false, 0, "")
	}
}

func invoiceFilename(title string) string {
	return fmt.Sprintf("%s.pdf", title)
}
