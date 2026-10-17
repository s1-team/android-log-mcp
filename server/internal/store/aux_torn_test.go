package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Issue #181: an entry appended after a torn last line starts a new line.
func TestAppendAuxAfterTornTail(t *testing.T) {
	for _, name := range []string{"crashes", "procs", "dropbox_seen"} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir(), "d", Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.AppendAux(name, map[string]int{"a": 1})
			fh, _ := os.OpenFile(filepath.Join(s.Dir, name+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
			fh.WriteString(`{"a":`) // killed in the middle of a write
			fh.Close()
			s.AppendAux(name, map[string]int{"a": 3})
			s.AppendAux(name, map[string]int{"a": 4}) // a normal append adds no blank line
			var got []string
			for _, l := range s.ReadAux(name) {
				got = append(got, string(l))
			}
			want := []string{`{"a":1}`, `{"a":`, `{"a":3}`, `{"a":4}`}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("lines %q, want %q", got, want)
			}
		})
	}
}

// A file that does not exist yet, and an empty one, get no leading newline.
func TestAppendAuxFirstLine(t *testing.T) {
	s, _ := Open(t.TempDir(), "d", Config{})
	defer s.Close()
	s.AppendAux("crashes", 1)
	os.WriteFile(filepath.Join(s.Dir, "procs.jsonl"), nil, 0o644)
	s.AppendAux("procs", 2)
	for n, w := range map[string]string{"crashes": "1\n", "procs": "2\n"} {
		if b, _ := os.ReadFile(filepath.Join(s.Dir, n+".jsonl")); string(b) != w {
			t.Errorf("%s = %q, want %q", n, b, w)
		}
	}
}
