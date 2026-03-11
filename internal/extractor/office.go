package extractor

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// extractDOCX extracts plain text from a DOCX file (OOXML ZIP archive).
// It reads word/document.xml and collects all <w:t> element values.
func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx: open zip: %w", err)
	}

	xmlData, err := readZipFile(zr, "word/document.xml")
	if err != nil {
		return "", fmt.Errorf("docx: read document.xml: %w", err)
	}

	text, err := extractWordXMLText(xmlData)
	if err != nil {
		return "", fmt.Errorf("docx: extract text: %w", err)
	}
	return text, nil
}

// extractWordXMLText parses a Word document.xml and extracts text from <w:t> elements,
// inserting paragraph breaks at <w:p> boundaries.
func extractWordXMLText(xmlData []byte) (string, error) {
	type wT struct {
		XMLName xml.Name `xml:"t"`
		Space   string   `xml:"space,attr"`
		Text    string   `xml:",chardata"`
	}

	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var sb strings.Builder
	var paraBuilder strings.Builder

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" && t.Name.Space != "" {
				// New paragraph: flush current paragraph.
				if paraBuilder.Len() > 0 {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(strings.TrimRight(paraBuilder.String(), " "))
					paraBuilder.Reset()
				}
			} else if t.Name.Local == "t" {
				var wt wT
				if err := dec.DecodeElement(&wt, &t); err != nil {
					continue
				}
				paraBuilder.WriteString(wt.Text)
			}
		}
	}

	// Flush any trailing paragraph content.
	if paraBuilder.Len() > 0 {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(strings.TrimRight(paraBuilder.String(), " "))
	}

	return strings.TrimSpace(sb.String()), nil
}

// extractXLSX extracts text from an XLSX file using the excelize library.
// Each cell value is tab-separated; rows are newline-separated; sheets are double-newline-separated.
func extractXLSX(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("xlsx: open: %w", err)
	}
	defer f.Close()

	sheets := f.GetSheetList()
	var sheetParts []string

	for _, sheet := range sheets {
		rows, err := f.GetRows(sheet)
		if err != nil {
			// Log and continue with remaining sheets.
			continue
		}

		var rowLines []string
		for _, row := range rows {
			line := strings.Join(row, "\t")
			if strings.TrimSpace(line) != "" {
				rowLines = append(rowLines, line)
			}
		}

		if len(rowLines) > 0 {
			sheetParts = append(sheetParts, strings.Join(rowLines, "\n"))
		}
	}

	return strings.Join(sheetParts, "\n\n"), nil
}

// extractPPTX extracts text from a PPTX file (OOXML ZIP archive).
// It iterates ppt/slides/slide*.xml files in order, extracting <a:t> element values.
func extractPPTX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("pptx: open zip: %w", err)
	}

	// Collect slide files and sort by slide number.
	type slideFile struct {
		num  int
		file *zip.File
	}
	var slides []slideFile

	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, "ppt/slides/slide") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		// Extract the number from "ppt/slides/slideN.xml".
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "ppt/slides/slide"), ".xml")
		num, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		slides = append(slides, slideFile{num: num, file: f})
	}

	sort.Slice(slides, func(i, j int) bool {
		return slides[i].num < slides[j].num
	})

	var slideParts []string

	for _, s := range slides {
		xmlData, err := readZipEntry(s.file)
		if err != nil {
			continue
		}

		text, err := extractPresentationXMLText(xmlData)
		if err != nil {
			continue
		}

		if strings.TrimSpace(text) != "" {
			slideParts = append(slideParts, text)
		}
	}

	return strings.Join(slideParts, "\n\n---\n\n"), nil
}

// extractPresentationXMLText parses a PowerPoint slide XML and extracts text from <a:t> elements.
func extractPresentationXMLText(xmlData []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var sb strings.Builder

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					continue
				}
				if text != "" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(text)
				}
			}
		}
	}

	return strings.TrimSpace(sb.String()), nil
}

// readZipFile reads a named file from a zip.Reader.
func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return readZipEntry(f)
		}
	}
	return nil, fmt.Errorf("file %q not found in archive", name)
}

// readZipEntry reads the contents of a zip.File entry.
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open zip entry %q: %w", f.Name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read zip entry %q: %w", f.Name, err)
	}
	return data, nil
}
