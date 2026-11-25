package forwarder

import (
	"strings"
	"testing"
	"time"
)

// These fixtures follow the terminal file the installed Cursor client writes
// (header rewritten in place while running, footer appended on exit).
const runningTerminalFile = "---\n" +
	"pid: 3300\n" +
	"cwd: \"c:\\\\Users\\\\dev\\\\project\"\n" +
	"command: \"npm run build\"\n" +
	"title: \"Build the project\"\n" +
	"status: running  \n" +
	"started_at: 2026-11-25T08:38:22.780Z\n" +
	"running_for_ms: 10014    \n" +
	"---\n" +
	"compiling module a\n" +
	"compiling module b\n"

const finishedTerminalFile = "---\n" +
	"pid: 2992\n" +
	"cwd: \"c:\\\\Users\\\\dev\\\\project\"\n" +
	"command: \"go test ./...\"\n" +
	"title: \"Run tests\"\n" +
	"status: succeeded\n" +
	"started_at: 2026-11-25T08:49:31.945Z\n" +
	"running_for_ms: 18567    \n" +
	"---\n" +
	"\n" +
	"ok  \tpkg/a\t0.579s\n" +
	"ok  \tpkg/b\t0.404s\n" +
	"\n" +
	"---\n" +
	"exit_code: 0\n" +
	"elapsed_ms: 18566\n" +
	"ended_at: 2026-11-25T08:49:50.511Z\n" +
	"---\n"

func TestParseTerminalFileRunningHasNoExitAndExposesHeader(t *testing.T) {
	view := parseTerminalFileView(runningTerminalFile)

	if view.Complete {
		t.Fatal("a file without a footer must not be reported complete")
	}
	if view.ExitCode != nil {
		t.Fatalf("ExitCode = %d, want none while running", *view.ExitCode)
	}
	if view.Status != "running" {
		t.Fatalf("Status = %q, want running", view.Status)
	}
	if view.RunningForMS == nil || *view.RunningForMS != 10014 {
		t.Fatalf("RunningForMS = %v, want 10014", view.RunningForMS)
	}
	wantStart := time.Date(2026, 11, 25, 8, 38, 22, 780_000_000, time.UTC)
	if !view.StartedAt.Equal(wantStart) {
		t.Fatalf("StartedAt = %s, want %s", view.StartedAt, wantStart)
	}
	if view.PID == nil || *view.PID != 3300 {
		t.Fatalf("PID = %v, want 3300", view.PID)
	}
	if view.CWD != `c:\Users\dev\project` {
		t.Fatalf("CWD = %q, want the unquoted path", view.CWD)
	}
	if view.Body != "compiling module a\ncompiling module b\n" {
		t.Fatalf("Body = %q, want only command output", view.Body)
	}
}

func TestParseTerminalFileFinishedReportsExitCodeAndElapsed(t *testing.T) {
	view := parseTerminalFileView(finishedTerminalFile)

	if !view.Complete {
		t.Fatal("a file with an exit_code footer must be reported complete")
	}
	if view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0", view.ExitCode)
	}
	if view.ElapsedMS == nil || *view.ElapsedMS != 18566 {
		t.Fatalf("ElapsedMS = %v, want 18566", view.ElapsedMS)
	}
	if view.Status != "succeeded" {
		t.Fatalf("Status = %q, want succeeded", view.Status)
	}
	if strings.Contains(view.Body, "exit_code") || strings.Contains(view.Body, "running_for_ms") {
		t.Fatalf("Body must not contain header or footer metadata: %q", view.Body)
	}
	if !strings.Contains(view.Body, "ok  \tpkg/b\t0.404s") {
		t.Fatalf("Body lost command output: %q", view.Body)
	}
}

func TestParseTerminalFileNonZeroAndUnknownExitCodes(t *testing.T) {
	failed := strings.Replace(finishedTerminalFile, "exit_code: 0\n", "exit_code: -1073741510\n", 1)
	view := parseTerminalFileView(failed)
	if !view.Complete || view.ExitCode == nil || *view.ExitCode != -1073741510 {
		t.Fatalf("negative exit code: complete=%t exit=%v", view.Complete, view.ExitCode)
	}

	unknown := strings.Replace(finishedTerminalFile, "exit_code: 0\n", "exit_code: unknown\n", 1)
	view = parseTerminalFileView(unknown)
	if !view.Complete {
		t.Fatal("an 'exit_code: unknown' footer still means the command ended")
	}
	if view.ExitCode != nil {
		t.Fatalf("ExitCode = %d, want none for 'unknown'", *view.ExitCode)
	}
}

// Command output is free-form. A dash rule or a metadata-looking line inside
// the output must not be mistaken for the footer.
func TestParseTerminalFileIgnoresFooterLookalikesInsideOutput(t *testing.T) {
	content := "---\n" +
		"pid: 10\n" +
		"status: running  \n" +
		"running_for_ms: 500    \n" +
		"---\n" +
		"# Report\n" +
		"---\n" +
		"exit_code: 7\n" +
		"---\n" +
		"still printing\n"

	view := parseTerminalFileView(content)

	if view.Complete {
		t.Fatal("output that merely contains a dash rule and 'exit_code:' is not a footer")
	}
	if !strings.Contains(view.Body, "exit_code: 7") || !strings.Contains(view.Body, "still printing") {
		t.Fatalf("Body must keep the lookalike lines as output: %q", view.Body)
	}
}

// Output that ends in a dash-fenced block is still output: the client's footer
// always carries ended_at, so a fenced block without it is not the footer.
func TestParseTerminalFileRequiresEndedAtInFooter(t *testing.T) {
	content := "---\n" +
		"pid: 10\n" +
		"status: running  \n" +
		"running_for_ms: 500    \n" +
		"---\n" +
		"front matter follows\n" +
		"---\n" +
		"exit_code: 7\n" +
		"---\n"

	view := parseTerminalFileView(content)

	if view.Complete {
		t.Fatal("a trailing fenced block without ended_at must not end the wait")
	}
	if !strings.Contains(view.Body, "exit_code: 7") {
		t.Fatalf("Body must keep the fenced block as output: %q", view.Body)
	}
}

// The real footer is always the last block, even when the output itself
// printed something footer-shaped earlier (for example by cat-ing another
// terminal file).
func TestParseTerminalFileUsesTheLastFooterBlock(t *testing.T) {
	content := "---\n" +
		"pid: 10\n" +
		"status: failed\n" +
		"running_for_ms: 900    \n" +
		"---\n" +
		"first\n" +
		"---\n" +
		"exit_code: 0\n" +
		"elapsed_ms: 1\n" +
		"ended_at: 2026-01-01T00:00:00.000Z\n" +
		"---\n" +
		"second\n" +
		"\n" +
		"---\n" +
		"exit_code: 3\n" +
		"elapsed_ms: 900\n" +
		"ended_at: 2026-01-01T00:00:01.000Z\n" +
		"---\n"

	view := parseTerminalFileView(content)

	if !view.Complete || view.ExitCode == nil || *view.ExitCode != 3 {
		t.Fatalf("complete=%t exit=%v, want the trailing footer (exit 3)", view.Complete, view.ExitCode)
	}
	if !strings.Contains(view.Body, "second") || !strings.Contains(view.Body, "exit_code: 0") {
		t.Fatalf("Body must keep everything before the trailing footer: %q", view.Body)
	}
}

func TestParseTerminalFileToleratesWindowsLineEndings(t *testing.T) {
	view := parseTerminalFileView(strings.ReplaceAll(finishedTerminalFile, "\n", "\r\n"))

	if !view.Complete || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("complete=%t exit=%v, want exit 0 with CRLF line endings", view.Complete, view.ExitCode)
	}
	if view.ElapsedMS == nil || *view.ElapsedMS != 18566 {
		t.Fatalf("ElapsedMS = %v, want 18566", view.ElapsedMS)
	}
}

// A ranged read returns only the last lines of a long file: there is no
// header, but the footer and the recent output are still there.
func TestParseTerminalFileTailWindowWithoutHeader(t *testing.T) {
	tail := "line 9998\nline 9999\n\n---\nexit_code: 2\nelapsed_ms: 60000\nended_at: 2026-11-25T08:49:50.511Z\n---\n"

	view := parseTerminalFileView(tail)

	if view.HasHeader {
		t.Fatal("a tail window has no header")
	}
	if !view.Complete || view.ExitCode == nil || *view.ExitCode != 2 {
		t.Fatalf("complete=%t exit=%v, want exit 2", view.Complete, view.ExitCode)
	}
	if !strings.HasPrefix(view.Body, "line 9998\nline 9999") {
		t.Fatalf("Body = %q, want the recent output", view.Body)
	}
}

// A tail window starts wherever the last N lines start. When that is a dash
// rule the command itself printed (YAML documents, markdown), what follows is
// output, not a header -- and the footer after it must still end the wait.
func TestParseTerminalFileTailWindowStartingOnADashRule(t *testing.T) {
	tail := "---\n" +
		"kind: Deployment\n" +
		"metadata:\n" +
		"  name: web\n" +
		"\n" +
		"---\n" +
		"exit_code: 0\n" +
		"elapsed_ms: 60000\n" +
		"ended_at: 2026-11-25T08:49:50.511Z\n" +
		"---\n"

	view := parseTerminalFileView(tail)

	if !view.Complete || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("complete=%t exit=%v, want the footer recognized (exit 0)", view.Complete, view.ExitCode)
	}
	if view.HasHeader {
		t.Fatal("output that begins with a dash rule is not a terminal file header")
	}
	if !strings.Contains(view.Body, "kind: Deployment") || !strings.Contains(view.Body, "name: web") {
		t.Fatalf("Body must keep the command output: %q", view.Body)
	}
	if strings.Contains(view.Body, "exit_code") {
		t.Fatalf("Body must not contain the footer: %q", view.Body)
	}
}

// The same window while the command is still running: no footer, all output.
func TestParseTerminalFileTailWindowOfDashRuledOutputStillRunning(t *testing.T) {
	tail := "---\n" +
		"kind: Deployment\n" +
		"---\n" +
		"kind: Service\n"

	view := parseTerminalFileView(tail)

	if view.HasHeader || view.Complete {
		t.Fatalf("hasHeader=%t complete=%t, want plain running output", view.HasHeader, view.Complete)
	}
	if !strings.Contains(view.Body, "kind: Deployment") || !strings.Contains(view.Body, "kind: Service") {
		t.Fatalf("Body must keep both documents: %q", view.Body)
	}
}

// A window that starts a few lines into the file begins inside the header.
// Those lines are metadata: a pattern such as "running" must not match them,
// and they still tell how long the command has been running.
func TestParseTerminalFileTailWindowStartingInsideTheHeader(t *testing.T) {
	tail := "command: \"npm run dev 2>&1 | tee error.log\"\n" +
		"title: \"Start the dev server\"\n" +
		"status: running  \n" +
		"started_at: 2026-11-25T08:38:22.780Z\n" +
		"running_for_ms: 10014    \n" +
		"---\n" +
		"compiling module a\n" +
		"compiling module b\n"

	view := parseTerminalFileView(tail)

	if view.Body != "compiling module a\ncompiling module b\n" {
		t.Fatalf("Body = %q, want only command output", view.Body)
	}
	if view.Status != "running" {
		t.Fatalf("Status = %q, want running from the partial header", view.Status)
	}
	if view.RunningForMS == nil || *view.RunningForMS != 10014 {
		t.Fatalf("RunningForMS = %v, want 10014 from the partial header", view.RunningForMS)
	}
	if view.Complete {
		t.Fatal("a running command must not be reported complete")
	}
}

// Output that merely looks like "key: value" lines before a dash rule is
// output. Only the client's own header fields are metadata.
func TestParseTerminalFileKeepsKeyValueOutputBeforeADashRule(t *testing.T) {
	tail := "name: web\n" +
		"replicas: 3\n" +
		"---\n" +
		"name: api\n"

	view := parseTerminalFileView(tail)

	if view.Body != tail {
		t.Fatalf("Body = %q, want the output untouched", view.Body)
	}
}

func TestTerminalFilePathJoinsLikeTheClient(t *testing.T) {
	cases := []struct {
		name    string
		folder  string
		shellID string
		want    string
		ok      bool
	}{
		{"posix remote folder", "/home/dev/.cursor/projects/p/terminals", "447245", "/home/dev/.cursor/projects/p/terminals/447245.txt", true},
		{"windows folder keeps its own separators", `C:\Users\dev\.cursor\projects\p\terminals`, "447245", `C:\Users\dev\.cursor\projects\p\terminals/447245.txt`, true},
		{"trailing slash", "/tmp/terminals/", "7", "/tmp/terminals/7.txt", true},
		{"empty folder", "", "7", "", false},
		{"non numeric id cannot name a terminal file", "/tmp/terminals", "../etc/passwd", "", false},
		{"subagent style id", "/tmp/terminals", "9f3c-agent", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := terminalFilePathForShell(tc.folder, tc.shellID)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("terminalFilePathForShell(%q, %q) = %q, %t; want %q, %t", tc.folder, tc.shellID, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestEstimateShellRuntimeTrustsRunningForOverSkewedClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	sample := terminalRuntimeSample{RunningForMS: 20_000, ReadAt: now.Add(-2 * time.Second)}

	// Clocks agree: started 22.5s ago, header said 20s two seconds ago.
	agree := estimateShellRuntimeMS(now, now.Add(-22500*time.Millisecond), sample)
	if agree != 22500 {
		t.Fatalf("agreeing clocks: runtime = %d, want 22500 (started_at wins when consistent)", agree)
	}

	// Remote host clock is an hour off: started_at is useless, fall back to
	// the header counter plus the time since we read it.
	skewed := estimateShellRuntimeMS(now, now.Add(-time.Hour), sample)
	if skewed != 22000 {
		t.Fatalf("skewed clock: runtime = %d, want 22000 (running_for_ms + local delta)", skewed)
	}

	// No started_at at all.
	if got := estimateShellRuntimeMS(now, time.Time{}, sample); got != 22000 {
		t.Fatalf("missing started_at: runtime = %d, want 22000", got)
	}
}
