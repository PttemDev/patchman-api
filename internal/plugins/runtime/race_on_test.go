//go:build race

package runtime

// The race detector slows wazero by 10× or more; wall-clock bounds loosen.
const raceEnabled = true
