package store

// Session is one boot of the device as seen by the recorder.
type Session struct {
	BootID   string `json:"boot_id"`
	Boot     string `json:"boot"` // short id used in records/cursors
	FirstSeq uint64 `json:"first_seq"`
	StartNs  int64  `json:"start_ns"` // first record time
}

// Gap is a period where recording was interrupted.
type Gap struct {
	FromNs int64  `json:"from_ns"`
	ToNs   int64  `json:"to_ns"`
	Reason string `json:"reason"`
}

// Meta is persisted per device in meta.json.
type Meta struct {
	Serial         string    `json:"serial"`
	Model          string    `json:"model,omitempty"`
	Release        string    `json:"release,omitempty"`
	SDK            string    `json:"sdk,omitempty"`
	TZ             string    `json:"tz,omitempty"`        // persist.sys.timezone
	TZOffset       string    `json:"tz_offset,omitempty"` // date +%z (source of truth)
	Sessions       []Session `json:"sessions,omitempty"`
	Gaps           []Gap     `json:"gaps,omitempty"`
	LastTimeNs     int64     `json:"last_time_ns,omitempty"` // newest record time (for -T resume)
	LastBootID     string    `json:"last_boot_id,omitempty"`
	RecordingSince int64     `json:"recording_since_ns,omitempty"` // host time ns
	LastSeenHostNs int64     `json:"last_seen_host_ns,omitempty"`  // host time of the writer's last flush or heartbeat (every 30 s)
	IngestHostNs   int64     `json:"ingest_host_ns,omitempty"`     // host time the newest line was received
	ClockOffsetNs  int64     `json:"clock_offset_ns,omitempty"`    // device - host
	ClockHostNs    int64     `json:"clock_host_ns,omitempty"`      // host time ClockOffsetNs was measured
	CrashSeq       int       `json:"crash_seq,omitempty"`
	AnrSeq         int       `json:"anr_seq,omitempty"`
	State          string    `json:"state,omitempty"` // last known adb state
}

// BootShort makes the short boot id stored in each record.
func BootShort(bootID string) string {
	h := ""
	for _, c := range bootID {
		if c != '-' {
			h += string(c)
		}
		if len(h) == 8 {
			break
		}
	}
	if h == "" {
		return "0"
	}
	return h
}
