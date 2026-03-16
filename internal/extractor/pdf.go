package extractor

import (
	"fmt"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

// extractPDF extracts plain text from PDF bytes using a pure-Go parser.
// It writes data to a temp file (required by the library), iterates all pages,
// and returns concatenated text. Returns a placeholder if no text is found.
func extractPDF(data []byte) (string, error) {
	// The ledongthuc/pdf library requires a file path, so write to a temp file.
	tmp, err := os.CreateTemp("", "xylolabs-kb-pdf-*.pdf")
	if err != nil {
		return "", fmt.Errorf("pdf: create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := tmp.Write(data); err != nil {
		return "", fmt.Errorf("pdf: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("pdf: close temp file: %w", err)
	}

	f, r, err := pdf.Open(tmp.Name())
	if err != nil {
		return "", fmt.Errorf("pdf: open: %w", err)
	}
	defer f.Close()

	var sb strings.Builder
	numPages := r.NumPage()

	if numPages == 0 {
		return "", fmt.Errorf("pdf: document has no pages")
	}

	for i := 1; i <= numPages; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}

		text, err := page.GetPlainText(nil)
		if err != nil {
			// Log and continue — partial extraction is better than none.
			continue
		}

		if sb.Len() > 0 {
			sb.WriteString("\n--- Page ")
			sb.WriteString(fmt.Sprintf("%d", i))
			sb.WriteString(" ---\n")
		}
		sb.WriteString(text)
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "[PDF with no extractable text]", nil
	}
	return result, nil
}
