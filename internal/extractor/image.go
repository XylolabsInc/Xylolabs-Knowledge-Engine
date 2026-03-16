package extractor

import (
	"context"
	"fmt"
)

const imagePrompt = "Describe this image concisely. Extract any text visible in the image. If it's a screenshot, describe the UI elements and any visible text."

// extractImage uses the Gemini vision API to describe an image and extract visible text.
// If the Gemini client is nil, returns a placeholder string.
func (e *Extractor) extractImage(ctx context.Context, data []byte, mimeType string, filename string) (string, error) {
	if e.gemini == nil {
		if filename != "" {
			return fmt.Sprintf("[Image: %s]", filename), nil
		}
		return "[Image]", nil
	}

	if len(data) == 0 {
		if filename != "" {
			return fmt.Sprintf("[Image: %s (empty)]", filename), nil
		}
		return "[Image (empty)]", nil
	}

	text, err := e.gemini.GenerateFromImage(ctx, imagePrompt, data, mimeType)
	if err != nil {
		return "", fmt.Errorf("image: generate from image: %w", err)
	}
	return text, nil
}
