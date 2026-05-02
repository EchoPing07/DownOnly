//go:build !linux

package main

func setLowPriority() {
	// Windows: no-op, low priority not critical for background download
}
