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
	LastTimeNs     int64     `json:"last_time_ns,omitempty"`    // maximum device time stored
	ArrivalTimeNs  int64     `json:"arrival_time_ns,omitempty"` // device time of the newest arrival (for -T resume; issue #52)
	LastBootID     string    `json:"last_boot_id,omitempty"`
	RecordingSince int64     `json:"recording_since_ns,omitempty"` // host time ns
	LastSeenHostNs int64     `json:"last_seen_host_ns,omitempty"`  // host time of the writer's last flush or heartbeat (every 30 s)
	IngestHostNs   int64     `json:"ingest_host_ns,omitempty"`     // host time the newest line was received
	ClockOffsetNs  int64     `json:"clock_offset_ns,omitempty"`    // device - host
	ClockHostNs    int64     `json:"clock_host_ns,omitempty"`      // host time ClockOffsetNs was measured
	// AutoTime is `settings get global auto_time` read with the clock
	// ("1", "0"; empty: unknown).
	AutoTime string `json:"auto_time,omitempty"`
	// ClockJumps are the device clock jumps seen in the stored lines,
	// oldest first (issue #48; bounded, pruned with the segments).
	ClockJumps []ClockJump `json:"clock_jumps,omitempty"`
	CrashSeq   int         `json:"crash_seq,omitempty"`
	AnrSeq     int         `json:"anr_seq,omitempty"`
	State      string      `json:"state,omitempty"` // last known adb state
	// Addr is the last adb serial the device was reached at (connection
	// address only; Serial is the stable device ID since layout v1).
	Addr string `json:"addr,omitempty"`
	// IDSource says how Serial (the device ID) was derived.
	IDSource string `json:"id_source,omitempty"`
	// SeqFloor: the device's seqs continue above this value although no
	// segment here holds it (the recording moved here from another data
	// root, `set-root`; issue #9). Cursors stay monotonic across the move.
	SeqFloor uint64 `json:"seq_floor,omitempty"`
	// Holes are seq ranges that are not in this directory (recorded in
	// another root before the recording moved back here).
	Holes []SeqRange `json:"holes,omitempty"`
	// ResumeDedup holds the hashes (one per line, repeats kept) of the
	// lines stored in the previous root near the newest arrival's device
	// time: logcat -T <that time> repeats them after a set-root move, and
	// this directory does not have them to compare against. Ignored once
	// the newest arrival moves on.
	ResumeDedup *ResumeDedup `json:"resume_dedup,omitempty"`
}

// ClockJump is a change of the device clock relative to the host clock
// (device time − host time) by more than ClockJumpThreshold, or a step in
// the device time of consecutive lines that looks like one (issue #48).
type ClockJump struct {
	// Seq is the first stored line after the jump; lines stay in arrival
	// order, so device times jump between Seq-1 and Seq.
	Seq      uint64 `json:"seq"`
	HostNs   int64  `json:"host_ns,omitempty"`   // host time it was seen (0: unknown, e.g. backfill)
	DeltaNs  int64  `json:"delta_ns"`            // size: + forward, − back
	BeforeNs int64  `json:"before_ns,omitempty"` // device time before the jump (last line, or the clock reading)
	AfterNs  int64  `json:"after_ns,omitempty"`  // device time after it
	// Source: "measured" (device clock read), "live" (a live line's device
	// time against the host time it arrived), "lines" (device time of a
	// line far below the lines before it) or "possible" (device time of a
	// line more than an hour after the previous line: a forward jump, or a
	// long quiet period).
	Source string `json:"source"`
}

// ClockJumpThreshold: smaller changes are drift, delivery delay or the
// interleaving of log buffers, not a jump.
const ClockJumpThreshold = int64(60e9)

// maxClockJumps bounds Meta.ClockJumps.
const maxClockJumps = 200

// ResumeDedup: see Meta.ResumeDedup.
type ResumeDedup struct {
	TimeNs int64    `json:"time_ns"`
	Hashes []uint64 `json:"hashes"`
}

// SeqRange is an inclusive range of seqs.
type SeqRange struct {
	First uint64 `json:"first"`
	Last  uint64 `json:"last"`
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

// ArrivalNs is the device time of the newest stored line (the -T resume
// point). Stores written before issue #52 only have LastTimeNs.
func (m *Meta) ArrivalNs() int64 {
	if m.ArrivalTimeNs != 0 {
		return m.ArrivalTimeNs
	}
	return m.LastTimeNs
}
