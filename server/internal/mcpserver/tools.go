package mcpserver

type toolDef struct {
	name, title, desc, schema string
}

type statusArgs struct {
	Device    string `json:"device"`
	Package   string `json:"package"`
	RecordFor string `json:"record_for"`
	Verbose   bool   `json:"verbose"`
}

type searchArgs struct {
	Device   string   `json:"device"`
	Filter   string   `json:"filter"`
	Package  string   `json:"package"`
	Tags     []string `json:"tags"`
	MinLevel string   `json:"min_level"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	View     string   `json:"view"`
	Limit    int      `json:"limit"`
	Context  int      `json:"context"`
	Before   string   `json:"before"`
	After    string   `json:"after"`
	WaitMs   int      `json:"wait_ms"`
	Stack    string   `json:"stack"`
	Verbose  bool     `json:"verbose"`
}

type crashArgs struct {
	Device  string   `json:"device"`
	Package string   `json:"package"`
	Kinds   []string `json:"kinds"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	ID      string   `json:"id"`
	Limit   int      `json:"limit"`
	// BeforeLines: lines of the crashed process before the crash in a detail
	BeforeLines int  `json:"before_lines"`
	Verbose     bool `json:"verbose"`
}

type launchArgs struct {
	Device  string `json:"device"`
	Package string `json:"package"`
	From    string `json:"from"`
	To      string `json:"to"`
	Limit   int    `json:"limit"`
	Verbose bool   `json:"verbose"`
}

func toolDefs() []toolDef {
	return []toolDef{
		{
			name:  "logcat_status",
			title: "Logcat recorder status",
			desc: "Starts recording the device, or extends it with record_for (max 24h; \"0\" stops). " +
				"Then reports adb/device/recording state and a `cursor` for the newest stored line (pass it as `after` to logcat_search to see only what happens next); call it first if unsure which device is connected.\n" +
				"Recording starts on use: the first log tool call for a device backfills its log buffer (up to 10 s). If the response says backfill in progress, call logcat_status again before using a cursor (none is returned until it has caught up).\n" +
				"A device stops recording after a while without log tool calls; record_for keeps it recording until a deadline.\n" +
				"With several connected devices and no `device`, nothing is started: pass device=<id>.\n" +
				"With `package`, also reports install state and live/recently-dead processes (pid, name, start/end), including com.foo:remote.\n" +
				"Response (fmt=1): `ok`/`empty` + key=value header, then `key k=v` fact lines (adb, store, device, recording, window, ...) and `note`/`hint ... next:`. With `package`: `package` and `proc pid= name= state=` lines (ended capped at 6). See docs/reference.md.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial or device ID. Omit when only one device is connected."},
"record_for":{"type":"string","description":"Keep recording this long, independent of calls: a duration like 30m, 2h, 1h30m (max 24h; never shortens). \"0\" stops recording now and removes the pin."},
"package":{"type":"string","description":"Exact application id, e.g. com.example.app. Optional."},
"verbose":{"type":"boolean","default":false,"description":"Always show the rarely changing lines (root, linger, lifetime, service log, adb, store); by default only on the first call of a session and when they change."}
},"additionalProperties":false}`,
		},
		{
			name:  "logcat_search",
			title: "Search recorded logcat",
			desc: "Search recorded logcat lines (buffers main, system, crash, events) of one device. Returns matching lines oldest to newest in a compact format; by default the NEWEST `limit` matches.\n" +
				"`filter` (Android Studio Logcat query) and the structured params (package, tags, min_level, from, to) are ANDed.\n" +
				"Filter syntax: key:value contains (case-insensitive), key=:value exact, key~:regex (RE2), -key:value negates. Keys: tag message package process line level age is, plus pid tid buffer. level:error = error and above. age:5m. is:crash, is:stacktrace. Bare words search the whole line.\n" +
				"`&` AND, `|` OR (spaces around them), parentheses group. Space-separated terms are ANDed, except repeated positive terms on one key are ORed (`tag:A tag:B` = A OR B). Quote values with spaces or colons: message:\"Start proc\".\n" +
				"Examples:\n  tag=:OkHttp tag=:MyRepo level:debug\n  is:crash | tag~:\"^am_(anr|crash)$\"\n" +
				"`package` is an EXACT application id covering all its processes (the filter's package: is a substring match).\n" +
				"Response (fmt=1): `ok`/`empty`/`error` + key=value header (matched, exact, shown, range, ...); rows `HH:MM:SS.mmm L pid/tid process tag: message` (`| ` continues, `^ rep=N` repeats). Every cut is `truncated ... next: <params>`; `hint`/`error` lines end in `next:`, directly executable. See docs/reference.md.\n" +
				"Paging: `newer: after=...` and (when cut) `next: before=...`. To wait for new lines, pass `after` plus `wait_ms` (max 25000).\n" +
				"The first call for a device starts recording and backfills its buffer (waits up to 10 s); `note code=backfill` means results may be incomplete: call again.\n" +
				"Java stacks keep their first 3 frames; stack=\"full\" shows every frame.\n" +
				"Use view=\"summary\" first for broad or unknown ranges.\n" +
				"Times are device time. After a device clock jump, a `clock_jumps=N splitting=S ... next: after=...` line appears when it splits the matches; with no from/to the newest lines by arrival are shown.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial. Omit when only one device has logs."},
"filter":{"type":"string","description":"Android Studio Logcat query, e.g. ` + "`tag=:OkHttp level:warn -message:heartbeat`" + `. Optional."},
"package":{"type":"string","description":"Exact application id; matches ALL its processes (com.foo, com.foo:remote), including dead ones."},
"tags":{"type":"array","items":{"type":"string"},"maxItems":20,"description":"Exact tag names, ORed together, e.g. [\"OkHttp\",\"MyRepo\"]."},
"min_level":{"type":"string","enum":["verbose","debug","info","warn","error","fatal"],"description":"Minimum priority."},
"from":{"type":"string","description":"Start, inclusive, device time: ` + "`2026-10-10 01:20:00`" + `, ` + "`01:20:00`" + ` (device today), ISO-8601 with offset, or relative ` + "`30m`, `2h`" + ` (device now minus that; the window ends at device now + 2 s). Host time is not supported."},
"to":{"type":"string","description":"End, exclusive, in device time. Same formats as ` + "`from`" + `. Default: no end (relative from: device now + 2 s)."},
"view":{"type":"string","enum":["lines","summary"],"default":"lines"},
"limit":{"type":"integer","minimum":1,"maximum":1000,"default":50,"description":"Max matching lines (output is also capped at ~30k chars); view=\"summary\" gives an overview in ~2k chars."},
"stack":{"type":"string","enum":["short","full"],"default":"short","description":"Java stacks in lines: short = first 3 frames per exception/Caused by, a repeated stack once (max 2,000 chars per line); full = every frame (max 8,000)."},
"context":{"type":"integer","minimum":0,"maximum":20,"default":0,"description":"Also show N lines before/after each match from the same process (grep -C)."},
"before":{"type":"string","description":"Cursor: return matches older than this (from a previous response)."},
"after":{"type":"string","description":"Cursor: return matches newer than this (from a previous response or logcat_status)."},
"wait_ms":{"type":"integer","minimum":0,"maximum":25000,"default":0,"description":"With ` + "`after`" + `: block up to this long until at least one new match arrives."},
"verbose":{"type":"boolean","default":false,"description":"Also print host_now, tz, the column legend, all clock jumps, shared-uid packages."}
},"additionalProperties":false}`,
		},
		{
			name:  "logcat_crashes",
			title: "Crashes and ANRs",
			desc: "List Java crashes, native crashes (tombstones) and ANRs, newest first, already parsed and grouped — use this instead of searching for \"FATAL EXCEPTION\" yourself.\n" +
				"Sources: crash/system/events buffers (FATAL EXCEPTION, native tombstones attributed to the crashed pid, am_anr + \"ANR in\") plus the device DropBox, which also covers crashes from before recording started and ANR thread dumps.\n" +
				"Pass `id` (e.g. \"cr3\") for full detail: complete stack / Caused-by chain, `before_lines` (default 20, max 100) log lines of the crashed process before the crash, and for ANRs the reason plus main-thread trace. If only one crash matches, detail is returned automatically.\n" +
				"Response (fmt=1): `ok`/`empty`/`error` + key=value header; `crash id=cr3 time= kind= pid= process=` entries, `same_signature ... next: id=...`; cuts are `truncated ... next:`. See docs/reference.md.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial. Omit when only one device has logs."},
"package":{"type":"string","description":"Exact application id. Omit for all apps."},
"kinds":{"type":"array","items":{"type":"string","enum":["java","native","anr"]},"description":"Default: all three."},
"from":{"type":"string","description":"Device time; same formats as logcat_search.from. Default: everything recorded."},
"to":{"type":"string","description":"Device time; same formats as logcat_search.to."},
"id":{"type":"string","description":"Crash id from a previous list (e.g. cr3, anr1) to get full detail."},
"limit":{"type":"integer","minimum":1,"maximum":50,"default":10},
"before_lines":{"type":"integer","minimum":1,"maximum":100,"default":20,"description":"With id: log lines before the crash."},
"verbose":{"type":"boolean","default":false,"description":"Also print host_now, tz, the ANR CPU block, shared-uid packages."}
},"additionalProperties":false}`,
		},
		{
			name:  "logcat_app_launches",
			title: "App launch timings",
			desc: "Report recent app launches with computed timings — use this for \"how long does cold start take / time to first onResume\".\n" +
				"Per launch: cold/warm/hot, activity, and a timeline relative to the launch request: process start, onCreate, onStart, onResume, Displayed (TTID) and Fully drawn (TTFD, only if the app calls reportFullyDrawn()).\n" +
				"Only reads logs: launch the app yourself first (e.g. `adb shell am force-stop <pkg>` then `adb shell am start -W -n <pkg>/<activity>`) or ask the user to tap the icon, then call this.\n" +
				"Response (fmt=1): `ok`/`empty` + header; `#N launch time= kind= component= pid= ttid_ms= ttfd_ms=` then `step name= at_ms= [took_ms=]` lines (at_ms: relative to the request); `unreliable=clock_jump` marks steps logged after a clock jump. See docs/reference.md.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial. Omit when only one device has logs."},
"package":{"type":"string","description":"Exact application id. Omit for all apps."},
"from":{"type":"string","description":"Device time. Default: 30m (device now minus 30 minutes, up to device now + 2 s). Same formats as logcat_search.from."},
"to":{"type":"string","description":"Device time; same formats as logcat_search.to."},
"limit":{"type":"integer","minimum":1,"maximum":20,"default":5},
"verbose":{"type":"boolean","default":false,"description":"Also print host_now and tz."}
},"additionalProperties":false}`,
		},
	}
}
