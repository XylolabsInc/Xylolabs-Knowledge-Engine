package extractor

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hanpama/hwp"
)

// extractHWPX extracts plain text from a HWPX file (ZIP archive with XML content).
// Uses the native Go hanpama/hwp library first, with custom XML parser as fallback.
func extractHWPX(data []byte) (string, error) {
	// Try native Go parser first (hanpama/hwp).
	var buf bytes.Buffer
	r := bytes.NewReader(data)
	if err := hwp.ReadHWPX(r, int64(len(data)), &buf); err == nil {
		result := strings.TrimSpace(buf.String())
		if result != "" {
			return result, nil
		}
	}

	// Fall back to custom XML parser.
	return extractHWPXFallback(data)
}

// extractHWPXFallback parses HWPX ZIP+XML directly as a fallback.
func extractHWPXFallback(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("hwpx: open zip: %w", err)
	}

	// Collect Contents/section*.xml files and sort by section number.
	type sectionFile struct {
		num  int
		file *zip.File
	}
	var sections []sectionFile

	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, "Contents/section") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		// Extract the number from "Contents/sectionN.xml".
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "Contents/section"), ".xml")
		num, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		sections = append(sections, sectionFile{num: num, file: f})
	}

	sort.Slice(sections, func(i, j int) bool {
		return sections[i].num < sections[j].num
	})

	var sectionParts []string

	for _, s := range sections {
		xmlData, err := readZipEntry(s.file)
		if err != nil {
			continue
		}

		text, err := extractHWPXSectionText(xmlData)
		if err != nil {
			continue
		}

		if strings.TrimSpace(text) != "" {
			sectionParts = append(sectionParts, text)
		}
	}

	result := strings.TrimSpace(strings.Join(sectionParts, "\n\n"))
	if result == "" {
		return "[HWPX with no extractable text]", nil
	}
	return result, nil
}

// extractHWPXSectionText parses a HWPX section XML and extracts text from <hp:t> elements,
// inserting paragraph breaks at <hp:p> boundaries and tabs between table cells.
func extractHWPXSectionText(xmlData []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var sb strings.Builder
	var paraBuilder strings.Builder

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("hwpx: xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				// New paragraph: flush current paragraph.
				if paraBuilder.Len() > 0 {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(strings.TrimRight(paraBuilder.String(), " "))
					paraBuilder.Reset()
				}
			case "tc":
				// Table cell: insert tab separator between cells.
				if paraBuilder.Len() > 0 {
					paraBuilder.WriteByte('\t')
				}
			case "t":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					continue
				}
				paraBuilder.WriteString(text)
			}
		case xml.EndElement:
			if t.Name.Local == "tc" {
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

// extractHWP extracts plain text from a legacy HWP 5.0 file.
// Uses the native Go hanpama/hwp library first, with Python CLI fallback.
func extractHWP(data []byte) (string, error) {
	// Try native Go parser first (hanpama/hwp).
	var buf bytes.Buffer
	if err := hwp.ReadHWP(bytes.NewReader(data), &buf); err == nil {
		result := strings.TrimSpace(buf.String())
		if result != "" {
			return result, nil
		}
	}

	// Fall back to Python CLI tools.
	return extractHWPPython(data)
}

// extractHWPPython extracts text from HWP using Python CLI tools as a fallback.
func extractHWPPython(data []byte) (string, error) {
	tmp, err := os.CreateTemp("", "xylolabs-kb-hwp-*.hwp")
	if err != nil {
		return "", fmt.Errorf("hwp: create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := tmp.Write(data); err != nil {
		return "", fmt.Errorf("hwp: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("hwp: close temp file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpName := tmp.Name()

	// Try hwp5txt CLI (from pyhwp package).
	if out, err := runCommand(ctx, "hwp5txt", tmpName); err == nil {
		result := strings.TrimSpace(out)
		if result != "" {
			return result, nil
		}
	}

	// Try Python one-liner with gethwp.
	pyGethwp := fmt.Sprintf("import gethwp; print(gethwp.read_hwp(%q))", tmpName)
	if out, err := runCommand(ctx, "python3", "-c", pyGethwp); err == nil {
		result := strings.TrimSpace(out)
		if result != "" {
			return result, nil
		}
	}

	return "[HWP file: could not extract text. Install pyhwp: pip install pyhwp]", nil
}

// runCommand runs an external command with context and returns its stdout output.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	switch name {
	case "hwp5txt", "python3":
	default:
		return "", fmt.Errorf("hwp: command %q is not allowed", name)
	}
	// #nosec G204 -- command name is restricted to a small allowlist above.
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("hwp: run %q: %w", name, err)
	}
	return stdout.String(), nil
}
