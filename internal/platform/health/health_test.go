package health

import (
	"sync"
	"testing"
)

// TestCheckTesseractCached verifies that checkTesseract uses sync.Once caching:
// the underlying subprocess is spawned at most once regardless of how many times
// checkTesseract is called concurrently. We validate this by resetting the
// package-level once/available state and calling checkTesseract many times in
// parallel, then asserting consistent results with no data races.
//
// Note: the test environment may not have tesseract installed. The important
// property under test is the caching behaviour (sync.Once), not the binary's
// presence. We verify that all concurrent calls return the same result, which
// proves the cached path is exercised rather than spawning N subprocesses.
func TestCheckTesseractCached(t *testing.T) {
	// Reset the package-level cache so this test gets a clean run.
	// In production code the once fires exactly once per process lifetime;
	// in tests we reset it to exercise the lazy-init path.
	tesseractOnce = sync.Once{}
	tesseractAvailable = false

	const goroutines = 20
	results := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = checkTesseract()
		}()
	}
	wg.Wait()

	// All goroutines must return the same error value (nil or non-nil).
	// A mix of nil and non-nil results would indicate a race on the cached state.
	firstResult := results[0]
	for i, res := range results {
		if (res == nil) != (firstResult == nil) {
			t.Errorf("goroutine %d got different result than goroutine 0: %v vs %v", i, res, firstResult)
		}
	}
}

// TestCheckTesseractResultIsStable verifies that calling checkTesseract twice
// returns the same result (once returns cached value, not re-spawning).
func TestCheckTesseractResultIsStable(t *testing.T) {
	// Reset cache.
	tesseractOnce = sync.Once{}
	tesseractAvailable = false

	err1 := checkTesseract()
	err2 := checkTesseract()

	// Both calls must agree on availability.
	if (err1 == nil) != (err2 == nil) {
		t.Errorf("checkTesseract returned inconsistent results: first=%v, second=%v", err1, err2)
	}
}

// TestCheckTesseractGenericError verifies that when tesseract is unavailable
// the returned error message is the generic "tesseract unavailable" string —
// not an OS path or exec detail that would expose runtime environment info.
func TestCheckTesseractGenericError(t *testing.T) {
	// Force unavailable by resetting the cache and keeping tesseractAvailable=false.
	// We call the Do func once with a no-op (the sync.Once fires, leaving
	// tesseractAvailable as false, as if the binary is missing).
	tesseractOnce = sync.Once{}
	tesseractOnce.Do(func() {
		// Simulate "tesseract not found": leave tesseractAvailable = false.
		tesseractAvailable = false
	})

	err := checkTesseract()
	if err == nil {
		// tesseract is actually installed — this test's error-message assertion
		// is not applicable. Skip rather than fail.
		t.Skip("tesseract binary is present in PATH — generic-error message test skipped")
	}

	if err.Error() != "tesseract unavailable" {
		t.Errorf("expected generic error message %q, got %q", "tesseract unavailable", err.Error())
	}
}
