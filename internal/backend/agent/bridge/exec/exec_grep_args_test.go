package execbridge

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/proto"
)

func TestPatternlessFilesGrepIsAGlobSearch(t *testing.T) {
	raw := []byte(`{"path":"c:\\repo","glob":"**/*test*","outputMode":"files_with_matches"}`)
	args, err := DecodeGrepToolArgs(raw, "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if args.GetPattern() != "" || args.GetGlob() != "**/*test*" {
		t.Fatalf("args = %+v", args)
	}
	globArgs, err := DecodeGlobToolArgs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if globArgs.GetGlobPattern() != "**/*test*" || !strings.Contains(globArgs.GetTargetDirectory(), "repo") {
		t.Fatalf("glob = %+v", globArgs)
	}
}

func TestOpenNativeExecKeepsPatternlessGlobGrep(t *testing.T) {
	bridge := NewBridge()
	message, pending, err := bridge.OpenNativeExec(&agentv1.ExecServerMessage{
		Id:     9,
		ExecId: "upstream-grep",
		Message: &agentv1.ExecServerMessage_GrepArgs{
			GrepArgs: &agentv1.GrepArgs{
				Path:       proto.String(`c:\repo`),
				Glob:       proto.String("**/*test*"),
				OutputMode: proto.String("files_with_matches"),
			},
		},
	}, runtimecore.ToolInvocation{CallID: "call-1", ToolName: "Glob", ArgsJSON: []byte(`{"glob":"**/*test*"}`)})
	if err != nil {
		t.Fatal(err)
	}
	got := message.GetExecServerMessage().GetGrepArgs()
	if got.GetPattern() != "" || got.GetGlob() != "**/*test*" || got.GetOutputMode() != "files_with_matches" {
		t.Fatalf("forwarded grep = %+v", got)
	}
	if pending.ExecKind != "glob" || pending.ExecID == "upstream-grep" || pending.MessageID == 9 {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestEmptyGrepPatternUsesTheIDEMessage(t *testing.T) {
	_, err := DecodeGrepToolArgs([]byte(`{"path":"c:\\repo","outputMode":"content"}`), "call-1")
	if err == nil || err.Error() != `pattern is empty. Pass "." to match every line.` {
		t.Fatalf("err = %v", err)
	}
}
