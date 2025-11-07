//go:build !darwin

package main

func captureWindowSnapshot() ([]windowSnapshot, error) {
	return nil, errWindowEnumerationUnavailable
}
