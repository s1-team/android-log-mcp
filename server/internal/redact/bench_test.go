package redact

import (
	"strings"
	"testing"
)

// Throughput benchmarks for Text (#177). One op = one pass over the corpus
// (per-line) or one call on the whole joined text (bulk); MB/s is therefore
// the redaction throughput, lines/s is reported as a metric.

func benchRedactor(b *testing.B) *Redactor {
	r, err := New(Options{Home: "/home/alice"})
	if err != nil {
		b.Fatal(err)
	}
	return r
}

func benchLines(b *testing.B, lines []string) {
	r := benchRedactor(b)
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	b.SetBytes(int64(total))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range lines {
			_ = r.Text(l)
		}
	}
	b.ReportMetric(float64(len(lines))*float64(b.N)/b.Elapsed().Seconds(), "lines/s")
}

func benchBulk(b *testing.B, lines []string) {
	r := benchRedactor(b)
	text := strings.Join(lines, "\n")
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Text(text)
	}
}

func BenchmarkTextPerLine(b *testing.B) {
	b.Run("real", func(b *testing.B) { benchLines(b, realMessages(3000)) })
	b.Run("synth_clean", func(b *testing.B) { benchLines(b, synthLines(3000, 1, 0)) })
	b.Run("synth_mixed", func(b *testing.B) { benchLines(b, synthLines(3000, 2, 0.01)) })
	b.Run("synth_dense", func(b *testing.B) { benchLines(b, synthLines(1000, 3, 0.08)) })
}

func BenchmarkTextBulk(b *testing.B) {
	b.Run("real", func(b *testing.B) { benchBulk(b, realMessages(3000)) })
	b.Run("synth_clean", func(b *testing.B) { benchBulk(b, synthLines(3000, 1, 0)) })
	b.Run("synth_mixed", func(b *testing.B) { benchBulk(b, synthLines(3000, 2, 0.01)) })
}
