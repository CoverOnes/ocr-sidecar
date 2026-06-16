package health

import (
	"fmt"
	"os/exec"
)

// checkTesseract verifies that the tesseract binary is present and executable.
// It runs `tesseract --version` and returns an error if it fails.
func checkTesseract() error {
	//nolint:gosec // G204: tesseract binary path is a fixed constant, no user input
	cmd := exec.Command("tesseract", "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tesseract not available: %w", err)
	}

	return nil
}
