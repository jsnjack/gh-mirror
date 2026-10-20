//go:build race

package store

// raceEnabled skips tests that run separately without the race detector.
const raceEnabled = true
