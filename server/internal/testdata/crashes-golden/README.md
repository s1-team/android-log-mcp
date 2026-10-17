# crashes.jsonl goldens (issue #225)

`crashes.jsonl` files and `logcat_crashes` outputs written by the released
binaries, used by the compatibility tests (new reads old):

- `v0.9.0/` and `v0.8.1/`: built from the release tags, replaying
  `../api29/{java_crash,native_crash,anr_broadcast}.logcat.txt` and the
  `dropbox-*.txt` fixtures through `device.Pipeline`.
  - `session`: the java, native and ANR crashes, each with its DropBox entry.
  - `compacted`: the same after `CompactAux`.
  - `dupanr`: `anr_broadcast` with the `ANR in` block moved 147 s after
    `am_anr` (two ANR rows, the old 120 s window).
- `*.out.txt`: the list view (`package` filter and no filter) and the detail
  view of every id; `offset_s=` is host-clock dependent and normalised to `N`.
- The 0.8.1 and 0.9.0 `session` files differ only in host times (`ht`) and
  produce the same output.
