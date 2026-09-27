//go:build race

package frigolite

// raceEnabled is true when the test binary is built with -race. The race
// detector runs the engine 5-20x slower, which breaks wall-clock-sensitive
// grind pins (TestFTS4Merge4Automerge8Grind checkpoint schedule); those
// tests skip under race and hold their contracts in the normal profile
// (NA_EVIDENCE §FULL-SUITE-DRIFT.T33-close).
const raceEnabled = true
