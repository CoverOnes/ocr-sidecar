package health

import (
	"errors"
	"os/exec"
	"sync"
)

// tesseractOnce caches the result of the first tesseract availability check.
// Subsequent /readyz probes reuse the cached result instead of spawning a new
// subprocess on every probe, preventing DoS through probe amplification.
var (
	tesseractOnce      sync.Once
	tesseractAvailable bool
)

// checkTesseract verifies that the tesseract binary is present and executable.
// The result is cached after the first call via sync.Once; subsequent calls
// return the cached value without spawning another subprocess.
func checkTesseract() error {
	tesseractOnce.Do(func() {
		//nolint:gosec // G204: tesseract binary path is a fixed constant, no user input
		cmd := exec.Command("tesseract", "--version")
		tesseractAvailable = cmd.Run() == nil
	})

	if !tesseractAvailable {
		return errors.New("tesseract unavailable")
	}

	return nil
}
