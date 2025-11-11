//go:build !darwin

package main

func captureWindowSnapshot() ([]windowSnapshot, error) {
	return nil, errWindowEnumerationUnavailable
}

func fetchAXWindowTitle(pid int32, windowID uint64) (string, bool) {
	return "", false
}
