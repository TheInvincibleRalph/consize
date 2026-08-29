package report

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	pageW  = 612.0
	pageH  = 792.0
	margin = 42.0
)

type pdfColor struct {
	r float64
	g float64
	b float64
}

var (
	ink         = pdfColor{0.055, 0.071, 0.063}
	muted       = pdfColor{0.373, 0.404, 0.384}
	subtle      = pdfColor{0.690, 0.714, 0.694}
	canvas      = pdfColor{0.973, 0.980, 0.976}
	card        = pdfColor{1, 1, 1}
	line        = pdfColor{0.875, 0.898, 0.886}
	accent      = pdfColor{0.000, 0.735, 0.463}
	accentSoft  = pdfColor{0.886, 0.973, 0.937}
	warning     = pdfColor{0.941, 0.596, 0.118}
	warningSoft = pdfColor{1.000, 0.957, 0.847}
	danger      = pdfColor{0.816, 0.200, 0.200}
	dangerSoft  = pdfColor{0.996, 0.914, 0.914}
	neutralSoft = pdfColor{0.946, 0.953, 0.949}
	neutralText = pdfColor{0.243, 0.267, 0.255}
	headerDark  = pdfColor{0.030, 0.046, 0.039}
	headerTint  = pdfColor{0.031, 0.137, 0.096}
)

func PDF(s Summary) ([]byte, error) {
	doc := newPDFDoc("Consize savings report")
	p := doc.addPage()
	drawReportPage(p, s)
	return doc.bytes(), nil
}

func drawReportPage(p *pdfPage, s Summary) {
	p.fill(canvas)
	p.rect(0, 0, pageW, pageH, true)

	drawHeader(p, s)
	drawExecutiveSummary(p, s)
	drawRecommendations(p, s, 304)
	drawVerificationPanel(p, s, 126)
	drawFooter(p, s)
}

func drawHeader(p *pdfPage, s Summary) {
	p.fill(headerDark)
	p.rect(0, 672, pageW, 120, true)
	p.fill(headerTint)
	p.rect(0, 672, 262, 120, true)
	p.fill(headerDark)
	p.alphaRect(260, 672, 352, 120, 0.92)

	drawLogo(p, margin, 728)
	p.text(margin+38, 748, "con", 16, "bold", card)
	p.text(margin+68, 748, "Size", 16, "bold", accent)
	p.text(margin, 708, fmt.Sprintf("%d-day savings report", s.RangeDays), 27, "bold", card)
	p.text(margin, 684, fmt.Sprintf("%s - %s", shortDate(s.From), shortDate(s.To)), 10, "regular", subtle)

	p.pill(450, 733, 116, 25, "Generated "+s.GeneratedAt.UTC().Format("Jan 02, 15:04")+" UTC", neutralSoft, neutralText)
	if freshness := latestFreshness(s); freshness != "" {
		p.pill(450, 700, 116, 25, freshness, accentSoft, accent)
	}
}

func drawLogo(p *pdfPage, x, y float64) {
	p.fill(accent)
	p.roundRect(x, y, 24, 24, 6, true)
	p.fill(headerDark)
	p.roundRect(x+4.5, y+4.5, 15, 15, 4, true)
	p.text(x+8.2, y+7.0, "S", 12, "bold", accent)
}

func drawExecutiveSummary(p *pdfPage, s Summary) {
	p.card(margin, 518, 528, 120)
	p.text(margin+20, 612, "Executive summary", 13, "bold", ink)
	p.textWrapped(margin+20, 594, 482, 11, reportNarrative(s), 8.8, "regular", muted, 2)

	cardY := 536.0
	metricCard(p, margin+20, cardY, 112, "Realized", money(s.RealizedThisPeriodMonthlySavings), "monthly savings", accent)
	metricCard(p, margin+144, cardY, 112, "Pending", money(s.ProjectedMonthlySavings), "monthly savings", ink)
	metricCard(p, margin+268, cardY, 92, "Open", fmt.Sprintf("%d", s.PendingRecommendations), "recommendations", ink)
	metricCard(p, margin+372, cardY, 116, "Verification", verificationSummary(s), "runs this period", statusColor(s))
}

func metricCard(p *pdfPage, x, y, w float64, label, value, hint string, color pdfColor) {
	p.fill(neutralSoft)
	p.roundRect(x, y, w, 42, 8, true)
	p.text(x+10, y+27, label, 7.2, "bold", muted)
	p.text(x+10, y+12, value, 14, "bold", color)
	p.text(x+10, y+4, hint, 6.3, "regular", subtle)
}

func drawRecommendations(p *pdfPage, s Summary, bottomY float64) {
	topY := bottomY + 196
	p.card(margin, bottomY, 528, 196)
	p.text(margin+20, topY-30, "Top recommendations", 13, "bold", ink)
	p.text(margin+20, topY-48, "Current optimization opportunities ranked by projected monthly savings.", 8.8, "regular", muted)

	headerY := topY - 76
	p.text(margin+20, headerY, "Workload", 7.4, "bold", muted)
	p.text(margin+252, headerY, "Change", 7.4, "bold", muted)
	p.text(margin+442, headerY, "Savings/mo", 7.4, "bold", muted)
	p.stroke(line)
	p.line(margin+20, headerY-8, margin+508, headerY-8)

	rowY := headerY - 24
	if len(s.TopPendingRecommendations) == 0 {
		p.text(margin+20, rowY, "No pending recommendations in this range.", 9.5, "regular", muted)
		return
	}
	for i, rec := range s.TopPendingRecommendations {
		if i >= 5 {
			break
		}
		if i > 0 {
			p.stroke(line)
			p.line(margin+20, rowY+12, margin+508, rowY+12)
		}
		p.text(margin+20, rowY, fit(fmt.Sprintf("%s/%s", rec.Namespace, rec.WorkloadName), 30), 8.2, "bold", ink)
		p.pill(margin+196, rowY-6, 44, 16, strings.ToUpper(rec.Resource), neutralSoft, neutralText)
		p.text(margin+252, rowY, fit(fmt.Sprintf("%s to %s", rec.Current, rec.Proposed), 34), 8.0, "regular", muted)
		p.textRight(margin+508, rowY, money(rec.SavingsMonthly), 9, "bold", accent)
		rowY -= 18
	}
}

func drawVerificationPanel(p *pdfPage, s Summary, y float64) {
	p.card(margin, y, 528, 142)
	p.text(margin+20, y+112, "Verification and safety", 13, "bold", ink)
	p.textWrapped(margin+20, y+94, 480, 10, "Proof that applied changes remained safe, plus rollback activity.", 8.4, "regular", muted, 2)

	pillY := y + 56
	statusPill(p, margin+20, pillY, "Passed", s.VerifiedApplies, accent, accentSoft)
	statusPill(p, margin+136, pillY, "Failed", s.FailedVerifications, danger, dangerSoft)
	statusPill(p, margin+252, pillY, "Inconclusive", s.InconclusiveVerifications, warning, warningSoft)
	statusPill(p, margin+396, pillY, "Rollbacks", s.Rollbacks, neutralText, neutralSoft)

	p.text(margin+20, y+25, "Data quality", 8, "bold", muted)
	quality := "Telemetry status unavailable."
	if len(s.DataQuality) > 0 {
		quality = s.DataQuality[0]
	}
	p.textWrapped(margin+20, y+11, 220, 9, quality, 7.8, "regular", ink, 2)

	if len(s.RecentRollbacks) > 0 {
		p.text(margin+310, y+25, "Recent rollback", 8, "bold", muted)
		ev := s.RecentRollbacks[0]
		p.textWrapped(margin+310, y+11, 198, 9, fmt.Sprintf("#%d %s/%s: %s", ev.ID, ev.Namespace, ev.WorkloadName, asciiChange(ev.Change)), 7.8, "regular", ink, 2)
	}
}

func statusPill(p *pdfPage, x, y float64, label string, count int, color, bg pdfColor) {
	p.fill(bg)
	p.roundRect(x, y, 100, 30, 8, true)
	p.text(x+10, y+17, label, 7.2, "bold", muted)
	p.textRight(x+88, y+8, fmt.Sprintf("%d", count), 13, "bold", color)
}

func drawFooter(p *pdfPage, s Summary) {
	p.stroke(line)
	p.line(margin, 42, pageW-margin, 42)
	p.text(margin, 26, "Consize optimization report", 7.2, "regular", muted)
	p.textRight(pageW-margin, 26, fmt.Sprintf("Range: %d days", s.RangeDays), 7.2, "regular", muted)
}

func reportNarrative(s Summary) string {
	if s.PendingRecommendations == 0 {
		return "No pending recommendations are waiting for action. Continue monitoring workloads for new savings opportunities."
	}
	return fmt.Sprintf("%s in projected monthly savings is currently pending across %d recommendations. %s has been verified this period.",
		money(s.ProjectedMonthlySavings), s.PendingRecommendations, money(s.RealizedThisPeriodMonthlySavings))
}

func verificationSummary(s Summary) string {
	total := s.VerifiedApplies + s.FailedVerifications + s.InconclusiveVerifications
	if total == 0 {
		return "0"
	}
	return fmt.Sprintf("%d/%d", s.VerifiedApplies, total)
}

func statusColor(s Summary) pdfColor {
	if s.FailedVerifications > 0 {
		return danger
	}
	if s.InconclusiveVerifications > 0 {
		return warning
	}
	return accent
}

func latestFreshness(s Summary) string {
	if s.LatestUsageBucket == nil {
		return ""
	}
	return "Telemetry " + relativeAge(s.GeneratedAt.Sub(s.LatestUsageBucket.UTC()))
}

func relativeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Hour {
		m := int(math.Round(d.Minutes()))
		if m <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%dm ago", m)
	}
	h := int(math.Round(d.Hours()))
	return fmt.Sprintf("%dh ago", h)
}

func shortDate(t time.Time) string {
	return t.UTC().Format("Jan 2, 2006")
}

func asciiChange(s string) string {
	return strings.ReplaceAll(s, "→", "to")
}

func fit(s string, max int) string {
	s = sanitizePDFText(s)
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return strings.TrimSpace(s[:max-3]) + "..."
}

type pdfDoc struct {
	title string
	pages []*pdfPage
}

type pdfPage struct {
	content bytes.Buffer
}

func newPDFDoc(title string) *pdfDoc {
	return &pdfDoc{title: title}
}

func (d *pdfDoc) addPage() *pdfPage {
	p := &pdfPage{}
	d.pages = append(d.pages, p)
	return p
}

func (d *pdfDoc) bytes() []byte {
	if len(d.pages) == 0 {
		d.addPage()
	}

	pageKids := make([]string, 0, len(d.pages))
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"", // pages object is filled after page object numbers are known.
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>",
	}

	for _, p := range d.pages {
		pageObjNum := len(objects) + 1
		contentObjNum := pageObjNum + 1
		pageKids = append(pageKids, fmt.Sprintf("%d 0 R", pageObjNum))
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>", pageW, pageH, contentObjNum),
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", p.content.Len(), p.content.String()),
		)
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(pageKids, " "), len(d.pages))

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects)+1)
	offsets = append(offsets, 0)
	for i, obj := range objects {
		offsets = append(offsets, out.Len())
		out.WriteString(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", i+1, obj))
	}
	xref := out.Len()
	out.WriteString(fmt.Sprintf("xref\n0 %d\n", len(objects)+1))
	out.WriteString("0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		out.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}
	out.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R /Info << /Title (%s) >> >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, escapePDF(d.title), xref))
	return out.Bytes()
}

func (p *pdfPage) fill(c pdfColor) {
	p.content.WriteString(fmt.Sprintf("%.3f %.3f %.3f rg\n", c.r, c.g, c.b))
}

func (p *pdfPage) stroke(c pdfColor) {
	p.content.WriteString(fmt.Sprintf("%.3f %.3f %.3f RG\n", c.r, c.g, c.b))
}

func (p *pdfPage) lineWidth(w float64) {
	p.content.WriteString(fmt.Sprintf("%.2f w\n", w))
}

func (p *pdfPage) rect(x, y, w, h float64, fill bool) {
	op := "S"
	if fill {
		op = "f"
	}
	p.content.WriteString(fmt.Sprintf("%.2f %.2f %.2f %.2f re %s\n", x, y, w, h, op))
}

func (p *pdfPage) alphaRect(x, y, w, h, alpha float64) {
	// A lightweight visual overlay: approximate opacity by blending with the page header color.
	_ = alpha
	p.rect(x, y, w, h, true)
}

func (p *pdfPage) roundRect(x, y, w, h, r float64, fill bool) {
	k := 0.5522847498
	op := "S"
	if fill {
		op = "f"
	}
	p.content.WriteString(fmt.Sprintf("%.2f %.2f m\n", x+r, y))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f l\n", x+w-r, y))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f %.2f %.2f %.2f %.2f c\n", x+w-r+k*r, y, x+w, y+r-k*r, x+w, y+r))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f l\n", x+w, y+h-r))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f %.2f %.2f %.2f %.2f c\n", x+w, y+h-r+k*r, x+w-r+k*r, y+h, x+w-r, y+h))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f l\n", x+r, y+h))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f %.2f %.2f %.2f %.2f c\n", x+r-k*r, y+h, x, y+h-r+k*r, x, y+h-r))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f l\n", x, y+r))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f %.2f %.2f %.2f %.2f c\n", x, y+r-k*r, x+r-k*r, y, x+r, y))
	p.content.WriteString("h " + op + "\n")
}

func (p *pdfPage) line(x1, y1, x2, y2 float64) {
	p.lineWidth(0.8)
	p.content.WriteString(fmt.Sprintf("%.2f %.2f m %.2f %.2f l S\n", x1, y1, x2, y2))
}

func (p *pdfPage) card(x, y, w, h float64) {
	p.fill(card)
	p.roundRect(x, y, w, h, 12, true)
	p.stroke(line)
	p.lineWidth(0.8)
	p.roundRect(x, y, w, h, 12, false)
}

func (p *pdfPage) pill(x, y, w, h float64, label string, bg, fg pdfColor) {
	p.fill(bg)
	p.roundRect(x, y, w, h, h/2, true)
	p.textCentered(x+w/2, y+(h/2)-3.5, fit(label, int(w/4.1)), 7.4, "bold", fg)
}

func (p *pdfPage) text(x, y float64, s string, size float64, weight string, c pdfColor) {
	font := "F1"
	if weight == "bold" {
		font = "F2"
	}
	p.content.WriteString("BT\n")
	p.content.WriteString(fmt.Sprintf("/%s %.2f Tf\n", font, size))
	p.content.WriteString(fmt.Sprintf("%.3f %.3f %.3f rg\n", c.r, c.g, c.b))
	p.content.WriteString(fmt.Sprintf("%.2f %.2f Td\n", x, y))
	p.content.WriteString("(" + escapePDF(sanitizePDFText(s)) + ") Tj\n")
	p.content.WriteString("ET\n")
}

func (p *pdfPage) textWrapped(x, y, maxW, leading float64, s string, size float64, weight string, c pdfColor, maxLines int) {
	lines := wrapLines(s, maxW, size, weight, maxLines)
	for i, line := range lines {
		p.text(x, y-float64(i)*leading, line, size, weight, c)
	}
}

func (p *pdfPage) textRight(x, y float64, s string, size float64, weight string, c pdfColor) {
	p.text(x-textWidth(s, size, weight), y, s, size, weight, c)
}

func (p *pdfPage) textCentered(x, y float64, s string, size float64, weight string, c pdfColor) {
	p.text(x-textWidth(s, size, weight)/2, y, s, size, weight, c)
}

func textWidth(s string, size float64, weight string) float64 {
	multiplier := 0.50
	if weight == "bold" {
		multiplier = 0.54
	}
	return float64(len(sanitizePDFText(s))) * size * multiplier
}

func wrapLines(s string, maxW, size float64, weight string, maxLines int) []string {
	if maxLines <= 0 {
		maxLines = 1
	}
	words := strings.Fields(sanitizePDFText(s))
	if len(words) == 0 {
		return nil
	}
	lines := []string{}
	current := ""
	for _, word := range words {
		next := word
		if current != "" {
			next = current + " " + word
		}
		if textWidth(next, size, weight) <= maxW || current == "" {
			current = next
			continue
		}
		lines = append(lines, current)
		current = word
		if len(lines) == maxLines {
			break
		}
	}
	if len(lines) < maxLines && current != "" {
		lines = append(lines, current)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	if len(lines) == maxLines && len(words) > 0 {
		joined := strings.Join(lines, " ")
		original := strings.Join(words, " ")
		if len(joined) < len(original) {
			lines[len(lines)-1] = trimToWidth(lines[len(lines)-1], maxW, size, weight)
		}
	}
	return lines
}

func trimToWidth(s string, maxW, size float64, weight string) string {
	s = strings.TrimSpace(s)
	if textWidth(s, size, weight) <= maxW {
		return s
	}
	for len(s) > 0 && textWidth(s+"...", size, weight) > maxW {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	if s == "" {
		return "..."
	}
	return s + "..."
}

func sanitizePDFText(s string) string {
	replacements := map[string]string{
		"→": "to",
		"—": "-",
		"–": "-",
		"•": "-",
		"·": "-",
		"“": "\"",
		"”": "\"",
		"’": "'",
		"‘": "'",
	}
	for old, next := range replacements {
		s = strings.ReplaceAll(s, old, next)
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r <= 126 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func escapePDF(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "(", "\\(")
	s = strings.ReplaceAll(s, ")", "\\)")
	return s
}
