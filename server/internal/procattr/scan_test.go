package procattr

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	execCall = regexp.MustCompile(`exec\.Command(Context)?\(`)
	hidden   = regexp.MustCompile(`procattr\.Hide\(exec\.Command(Context)?\(`)
	// other ways to start a process, which would bypass Hide entirely
	other = regexp.MustCompile(`exec\.Cmd\{|os\.StartProcess\(|syscall\.StartProcess\(|windows\.CreateProcess\(`)
	alias = regexp.MustCompile(`^([A-Za-z_]\w*|\.)\s+"os/exec"`)
)

// TestEveryChildIsHidden scans the server's non-test Go sources: every
// exec.Command / exec.CommandContext must be wrapped in procattr.Hide, so no
// child process opens a console window on Windows.
func TestEveryChildIsHidden(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("server root not found from %s: %v", root, err)
	}
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "third_party_ref" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files++
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.Contains(line, "procattr:allow") {
				continue
			}
			imp := strings.TrimPrefix(strings.TrimSpace(line), "import ")
			if other.MatchString(line) || alias.MatchString(strings.TrimSpace(imp)) {
				t.Errorf("%s:%d: start child processes with procattr.Hide(exec.Command…(…)) (no aliased os/exec, exec.Cmd literals or StartProcess):\n\t%s", p, i+1, strings.TrimSpace(line))
			}
			if n := len(execCall.FindAllString(line, -1)); n > len(hidden.FindAllString(line, -1)) {
				t.Errorf("%s:%d: wrap the child in procattr.Hide(exec.Command…(…)) so it gets no console window on Windows:\n\t%s", p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 {
		t.Fatalf("scanned only %d files; wrong root?", files)
	}
}

// TestLauncherSpawnsDirectly: the npm launcher must start the Go binary
// directly (no shell, so no cmd.exe on Windows) with windowsHide.
func TestLauncherSpawnsDirectly(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "npm", "android-log-mcp", "bin", "android-log-mcp.js"))
	if err != nil {
		t.Skip("npm launcher not found:", err)
	}
	src := string(b)
	spawn := regexp.MustCompile(`spawn\(bin, [^\n]*\{([^}]*)\}\)`).FindStringSubmatch(src)
	if spawn == nil {
		t.Fatal("launcher: spawn(bin, …, {…}) not found")
	}
	if !strings.Contains(spawn[1], "windowsHide: true") || !strings.Contains(spawn[1], "shell: false") {
		t.Errorf("launcher spawn options %q: want windowsHide: true and shell: false", spawn[1])
	}
	if strings.Contains(src, "shell: true") || strings.Contains(src, "execSync") || strings.Contains(src, "exec(") {
		t.Error("launcher must not start the binary through a shell")
	}
}
