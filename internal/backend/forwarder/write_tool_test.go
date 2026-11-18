package forwarder

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/encoding/protojson"
)

func TestComputeEditDiffEmitsUnescapedMultilineUnifiedDiffForNewFile(t *testing.T) {
	after := "Line 1\nLine 2\nLine 3\nLine 4\nLine 5\nLine 6\nLine 7\nLine 8\nLine 9\nLine 10\n"
	diffString, linesAdded, linesRemoved := computeEditDiff("", after)
	if linesAdded != 10 || linesRemoved != 0 {
		t.Fatalf("linesAdded=%d linesRemoved=%d, want 10 and 0", linesAdded, linesRemoved)
	}
	if strings.Contains(diffString, "%0A") {
		t.Fatalf("diffString must not URL-encode newlines as %%0A: %q", diffString)
	}
	want := strings.Join([]string{
		"@@ -0,0 +1,10 @@",
		"+Line 1",
		"+Line 2",
		"+Line 3",
		"+Line 4",
		"+Line 5",
		"+Line 6",
		"+Line 7",
		"+Line 8",
		"+Line 9",
		"+Line 10",
	}, "\n")
	if diffString != want {
		t.Fatalf("diffString =\n%s\nwant:\n%s", diffString, want)
	}

	result := buildSuccessfulWriteResult(`C:\repo\test10.txt`, "", after)
	success := result.GetSuccess()
	if success == nil {
		t.Fatal("expected EditResult_Success")
	}
	if success.BeforeFullFileContent != nil {
		t.Fatalf("new file BeforeFullFileContent must stay nil so Cursor IDE tracks newlyCreatedFiles, got %q", success.GetBeforeFullFileContent())
	}
	if success.GetDiffString() != want {
		t.Fatalf("EditSuccess.DiffString = %q, want %q", success.GetDiffString(), want)
	}
}

func TestComputeEditDiffHandlesNoTrailingNewlineAndSpecialCharacters(t *testing.T) {
	after := "first %0A literal\n@@ not a header @@\n--- literal"
	diffString, linesAdded, linesRemoved := computeEditDiff("", after)
	if linesAdded != 3 || linesRemoved != 0 {
		t.Fatalf("linesAdded=%d linesRemoved=%d, want 3 and 0", linesAdded, linesRemoved)
	}
	want := strings.Join([]string{
		"@@ -0,0 +1,3 @@",
		"+first %0A literal",
		"+@@ not a header @@",
		"+--- literal",
		`\ No newline at end of file`,
	}, "\n")
	if diffString != want {
		t.Fatalf("diffString =\n%s\nwant:\n%s", diffString, want)
	}
}

func TestComputeEditDiffMultiHunkExistingFileAndCRLFNormalization(t *testing.T) {
	beforeLines := make([]string, 12)
	afterLines := make([]string, 12)
	for i := 0; i < 12; i++ {
		line := "line " + string(rune('A'+i))
		beforeLines[i] = line
		afterLines[i] = line
	}
	afterLines[1] = "updated B"
	afterLines[10] = "updated K"

	before := strings.Join(beforeLines, "\n") + "\n"
	afterCRLF := strings.Join(afterLines, "\r\n") + "\r\n"

	diffString, linesAdded, linesRemoved := computeEditDiff(before, afterCRLF)
	if linesAdded != 2 || linesRemoved != 2 {
		t.Fatalf("linesAdded=%d linesRemoved=%d, want 2 and 2", linesAdded, linesRemoved)
	}
	if strings.Contains(diffString, "\r") || strings.Contains(diffString, "%") {
		t.Fatalf("unexpected carriage return or percent-escape in diffString: %q", diffString)
	}
	want := strings.Join([]string{
		"@@ -1,5 +1,5 @@",
		" line A",
		"-line B",
		"+updated B",
		" line C",
		" line D",
		" line E",
		"@@ -8,5 +8,5 @@",
		" line H",
		" line I",
		" line J",
		"-line K",
		"+updated K",
		" line L",
	}, "\n")
	if diffString != want {
		t.Fatalf("diffString =\n%s\nwant:\n%s", diffString, want)
	}
}

func TestDecodeWriteOperationArgsPreservesLeadingAndTrailingWhitespace(t *testing.T) {
	wantContents := "\n  indented first line\nsecond line\n\n"
	writeProto := &agentv1.WriteArgs{
		Path:       `C:\repo\sample.py`,
		FileText:   wantContents,
		ToolCallId: "toolu_write_1",
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: false}).Marshal(writeProto)
	if err != nil {
		t.Fatalf("protojson.Marshal(WriteArgs) error = %v", err)
	}
	decoded, err := decodeWriteOperationArgs(raw)
	if err != nil {
		t.Fatalf("decodeWriteOperationArgs error = %v", err)
	}
	if decoded.Contents != wantContents {
		t.Fatalf("Contents = %q, want %q", decoded.Contents, wantContents)
	}

	started := buildStartedToolCall(runtimecore.ToolInvocation{
		CallID:   "call-write-1",
		ToolName: "Write",
		ArgsJSON: raw,
	})
	if got := started.GetEditToolCall().GetArgs().GetStreamContent(); got != wantContents {
		t.Fatalf("started StreamContent = %q, want %q", got, wantContents)
	}
}

func TestDecodeWriteOperationArgsSupportsFileBytesFallback(t *testing.T) {
	wantContents := "alpha\nbeta\ngamma\n"
	raw, err := json.Marshal(map[string]any{
		"path":      `C:\repo\from_bytes.txt`,
		"fileBytes": base64.StdEncoding.EncodeToString([]byte(wantContents)),
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWriteOperationArgs(raw)
	if err != nil {
		t.Fatalf("decodeWriteOperationArgs(fileBytes) error = %v", err)
	}
	if decoded.Contents != wantContents {
		t.Fatalf("Contents = %q, want %q", decoded.Contents, wantContents)
	}
}
