//go:build !race

package frigolite

// raceEnabled is false in normal test builds; see frigolite_race_test.go.
const raceEnabled = false
