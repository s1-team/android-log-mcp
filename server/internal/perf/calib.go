package perf

import (
	"crypto/sha256"
	"encoding/json"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Calibrate times a fixed, stdlib-only CPU workload (hashing, sorting,
// JSON, regexp, string building: the same kinds of work as ingest and
// search, but none of this repository's code) and returns the median of 5
// runs in milliseconds. Timing metrics are compared after scaling by the
// ratio of the current run's calibration to the baseline's, so a slower or
// faster machine (CI runner vs. the machine that recorded the baseline)
// does not count as a regression or an improvement.
func Calibrate() float64 {
	runs := make([]float64, 5)
	for i := range runs {
		t0 := time.Now()
		calibWork()
		runs[i] = float64(time.Since(t0).Microseconds()) / 1000
	}
	sort.Float64s(runs)
	return runs[len(runs)/2]
}

var calibSink int

func calibWork() {
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, 4<<20)
	rng.Read(buf)
	for i := 0; i < 4; i++ {
		s := sha256.Sum256(buf)
		calibSink += int(s[0])
	}
	ints := make([]int, 200_000)
	for i := range ints {
		ints[i] = rng.Int()
	}
	sort.Ints(ints)
	type rec struct {
		S uint64 `json:"s"`
		G string `json:"g"`
		M string `json:"m"`
	}
	var b strings.Builder
	for i := 0; i < 20_000; i++ {
		x, _ := json.Marshal(rec{uint64(i), "Tag" + strconv.Itoa(i%50), "message number " + strconv.Itoa(i) + " with some payload text"})
		var y rec
		json.Unmarshal(x, &y)
		b.WriteString(y.M)
		b.WriteByte('\n')
	}
	re := regexp.MustCompile(`number \d+7 with`)
	calibSink += len(re.FindAllStringIndex(b.String(), -1))
}
