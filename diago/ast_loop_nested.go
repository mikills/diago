package diago

// External operations are retained per variable so conflict proofs can cross
// known invocation boundaries without contaminating unrelated captures.
const (
	loopExternalRead uint8 = 1 << iota // slot/header
	loopExternalWrite
	loopExternalDataRead
	loopExternalDataWrite
)
