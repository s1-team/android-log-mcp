package mcpserver

type toolDef struct {
	name, title, desc, schema string
}

type statusArgs struct {
	Device    string `json:"device"`
	Package   string `json:"package"`
	RecordFor string `json:"record_for"`
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
}

type crashArgs struct {
	Device  string   `json:"device"`
	Package string   `json:"package"`
	Kinds   []string `json:"kinds"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	ID      string   `json:"id"`
	Limit   int      `json:"limit"`
}

type launchArgs struct {
	Device  string `json:"device"`
	Package string `json:"package"`
	From    string `json:"from"`
	To      string `json:"to"`
	Limit   int    `json:"limit"`
}

func toolDefs() []toolDef {
	return []toolDef{
		{
			name:  "logcat_status",
			title: "Logcat recorder status",
			desc: "Starts recording the device, or extends it with record_for (max 24h; \"0\" stops). " +
				"Then reports adb/device/recording state and a `cursor` marking \"now\" (pass it as `after` to logcat_search to see only what happens next, e.g. while the user reproduces a bug); call it first if unsure which device is connected.\n" +
				"Recording starts on use: the first log tool call for a device backfills what is still in its log buffer, waiting up to 10 s. If the response says backfill in progress, call logcat_status again before using a cursor (no cursor is returned until the backfill has caught up).\n" +
				"Each device stops recording after a while without log tool calls (shown in the response); record_for keeps it recording until a deadline regardless of calls.\n" +
				"With several connected devices and no `device`, nothing is started: pass device=<id>.\n" +
				"With `package`, also reports install state and live/recently-dead processes (pid, process name, start/end time), including multi-process names like com.foo:remote.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial or device ID. Omit when only one device is connected."},
"record_for":{"type":"string","description":"Keep recording this device for this long, independent of calls: a duration with a unit like 30m, 2h or 1h30m (max 24h; the deadline never shortens). \"0\" stops recording the device now and removes the pin."},
"package":{"type":"string","description":"Exact application id, e.g. com.example.app. Optional."}
},"additionalProperties":false}`,
		},
		{
			name:  "logcat_search",
			title: "Search recorded logcat",
			desc: "Search recorded logcat lines (buffers main, system, crash, events) of one device. Returns matching lines oldest→newest in a compact format; by default the NEWEST `limit` matches.\n" +
				"Filtering = `filter` (Android Studio Logcat query syntax) AND the structured params (package, tags, min_level, from, to). All parts are ANDed.\n" +
				"Filter syntax (Android Studio): key:value contains (case-insensitive), key=:value exact, key~:regex (RE2, case-sensitive), -key:value negates. Keys: tag message package process line level age is, plus extensions pid tid buffer (main|system|crash|events). level:error = error and above (also accepts V D I W E F). age:5m. is:crash, is:stacktrace. Bare words search the whole line.\n" +
				"Combining: `&` AND, `|` OR (put spaces around them), parentheses group. Space-separated terms are ANDed, EXCEPT repeated positive terms on the same key, which are ORed: `tag:A tag:B` = A OR B; use `message:foo & message:bar` for AND. Quote values with spaces or colons: message:\"Start proc\".\n" +
				"Examples:\n  tag=:OkHttp tag=:MyRepo level:debug\n  is:crash | tag~:\"^am_(anr|crash)$\"\n  message~:\"Displayed com\\.foo\" -tag:chatty\n" +
				"Invalid filters return an error with the position and a fix; nothing is guessed. The response echoes how the filter was interpreted — check it.\n" +
				"The `package` param is an EXACT application id covering all its processes (filter's package: is a substring match).\n" +
				"Counts: the `count:` line has total and count_exact; count_exact=false means total is a lower bound counted back to counted_from (or up to counted_to): narrow with from/to for an exact count.\n" +
				"Paging: response gives `before`/`after` cursors. To wait for new lines (e.g. while the user reproduces), pass `after` plus `wait_ms` (≤25000).\n" +
				"The first call for a device starts recording it and backfills its log buffer (waits up to 10 s); a \"backfill in progress\" note means results may be incomplete: call again.\n" +
				"Use view=\"summary\" first for broad or unknown time ranges (counts by level/tag/package, top repeated messages).",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial. Omit when only one device has logs."},
"filter":{"type":"string","description":"Android Studio Logcat query, e.g. ` + "`tag=:OkHttp level:warn -message:heartbeat`" + `. Optional."},
"package":{"type":"string","description":"Exact application id; matches lines from ALL its processes (com.foo, com.foo:remote), including processes that have since died."},
"tags":{"type":"array","items":{"type":"string"},"maxItems":20,"description":"Exact tag names, ORed together, e.g. [\"OkHttp\",\"MyRepo\"]."},
"min_level":{"type":"string","enum":["verbose","debug","info","warn","error","fatal"],"description":"Minimum priority."},
"from":{"type":"string","description":"Start time, inclusive. Device-local ` + "`2026-10-10 01:20:00`" + ` or ` + "`01:20:00`" + ` (= device's today), ISO-8601 with offset, or relative like ` + "`30m`, `2h`" + ` (= device now minus that)."},
"to":{"type":"string","description":"End time, exclusive. Same formats as ` + "`from`" + `. Default: now."},
"view":{"type":"string","enum":["lines","summary"],"default":"lines"},
"limit":{"type":"integer","minimum":1,"maximum":1000,"default":200,"description":"Max matching lines to return (output is also capped at ~30k chars)."},
"context":{"type":"integer","minimum":0,"maximum":20,"default":0,"description":"Also show N lines before/after each match from the same process (like grep -C)."},
"before":{"type":"string","description":"Cursor: return matches older than this (from a previous response)."},
"after":{"type":"string","description":"Cursor: return matches newer than this (from a previous response or logcat_status)."},
"wait_ms":{"type":"integer","minimum":0,"maximum":25000,"default":0,"description":"With ` + "`after`" + `: block up to this long until at least one new match arrives."}
},"additionalProperties":false}`,
		},
		{
			name:  "logcat_crashes",
			title: "Crashes and ANRs",
			desc: "List Java crashes, native crashes (tombstones) and ANRs, newest first, already parsed and grouped — use this instead of searching for \"FATAL EXCEPTION\" yourself.\n" +
				"Sources: crash/system/events log buffers (AndroidRuntime FATAL EXCEPTION, libc \"Fatal signal\" + DEBUG tombstone lines attributed to the crashed pid, am_anr + \"ANR in\" block) plus the device DropBox (data_app_crash / data_app_native_crash / data_app_anr), which also covers crashes from before recording started and ANR thread dumps.\n" +
				"Pass `id` (e.g. \"cr3\") for full detail: complete stack / Caused-by chain, the crashed process's last 40 log lines before the crash, and for ANRs the reason plus main-thread trace (or why traces are unavailable). If only one crash matches, detail is returned automatically.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial. Omit when only one device has logs."},
"package":{"type":"string","description":"Exact application id. Omit for all apps."},
"kinds":{"type":"array","items":{"type":"string","enum":["java","native","anr"]},"description":"Default: all three."},
"from":{"type":"string","description":"Same formats as logcat_search.from. Default: everything recorded."},
"to":{"type":"string","description":"Same formats as logcat_search.to."},
"id":{"type":"string","description":"Crash id from a previous list (e.g. cr3, anr1) to get full detail."},
"limit":{"type":"integer","minimum":1,"maximum":50,"default":10}
},"additionalProperties":false}`,
		},
		{
			name:  "logcat_app_launches",
			title: "App launch timings",
			desc: "Report recent app launches with computed timings — use this for \"how long does cold start take / time to first onResume\".\n" +
				"For each launch: cold/warm/hot, the launched activity, and a timeline relative to the launch request: process start (am_proc_start), Activity onCreate, onStart, onResume (from wm_on_*_called events in the app process), Displayed (time to initial display, TTID) and Fully drawn (TTFD, only if the app calls reportFullyDrawn()).\n" +
				"This tool only reads logs; launch the app yourself first (e.g. `adb shell am force-stop <pkg>` then `adb shell am start -W -n <pkg>/<activity>`), or ask the user to tap the icon, then call this.",
			schema: `{"type":"object","properties":{
"device":{"type":"string","description":"adb serial. Omit when only one device has logs."},
"package":{"type":"string","description":"Exact application id. Omit for all apps."},
"from":{"type":"string","description":"Default: last 30 minutes. Same formats as logcat_search.from."},
"to":{"type":"string","description":"Same formats as logcat_search.to."},
"limit":{"type":"integer","minimum":1,"maximum":20,"default":5}
},"additionalProperties":false}`,
		},
	}
}
