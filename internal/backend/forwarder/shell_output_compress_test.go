package forwarder

import (
	"strings"
	"testing"
)

func TestCompressShellOutputLeavesShortCommands(t *testing.T) {
	input := "/root/teapot\n M src/main.rs\n?? tools/\n"
	if got := compressShellOutput(input); got != strings.TrimSpace(input) {
		t.Fatalf("short output changed: %q", got)
	}
}

func TestCompressShellOutputCapsGitLog(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("cff2db")
		b.WriteByte('0' + byte(i%10))
		b.WriteString(" Fix the account queue and keep the long explanation so this line is not tiny ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteByte('\n')
	}
	got := compressShellOutput(b.String())
	if strings.Count(got, "cff2db") != 15 {
		t.Fatalf("kept %d git log lines, want 15\n%s", strings.Count(got, "cff2db4"), got)
	}
	if !strings.Contains(got, "older git log entries omitted") {
		t.Fatalf("missing omission note: %s", got)
	}
}

func TestCompressShellOutputDropsPassingTests(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("test_account_queue_")
		b.WriteByte('a' + byte(i%26))
		b.WriteString(strings.Repeat("p", 40))
		b.WriteString(" PASSED\n")
	}
	b.WriteString("test_view_pool FAILED\n")
	b.WriteString("1 failed, 30 passed\n")
	got := compressShellOutput(b.String())
	if strings.Contains(got, " PASSED") {
		t.Fatalf("passing lines kept: %s", got)
	}
	if !strings.Contains(got, "test_view_pool FAILED") || !strings.Contains(got, "1 failed, 30 passed") {
		t.Fatalf("failure or summary dropped: %s", got)
	}
}

func TestCompressShellOutputSamplesDirectoryListings(t *testing.T) {
	var b strings.Builder
	b.WriteString("total 4215960\n")
	for i := 0; i < 40; i++ {
		b.WriteString("-rw-rw----+   1 root root     18792 Nov 13 20:34 accounts_")
		b.WriteByte('a' + byte(i%26))
		b.WriteString(strings.Repeat("n", 20))
		b.WriteByte('\n')
	}
	got := compressShellOutput(b.String())
	if !strings.Contains(got, "total 4215960") || !strings.Contains(got, "directory entries omitted") {
		t.Fatalf("listing was not sampled: %s", got)
	}
	if strings.Count(got, "accounts_") != 20 {
		t.Fatalf("kept %d entries, want 20", strings.Count(got, "accounts_"))
	}
}
