//go:build race

package store

// decodeIters is the number of random records TestDecodeRecordMarshalled
// round-trips. The race detector slows the decoder about 20x and finds
// nothing more in a pure function, so the -race runs (the release race gate)
// use a tenth of the iterations; the plain `go test` run keeps all of them
// (issue #154).
const decodeIters = 20000
