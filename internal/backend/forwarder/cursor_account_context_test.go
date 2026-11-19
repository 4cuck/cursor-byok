package forwarder

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/encoding/protojson"
)

func TestCursorAccountForwardContextKeepsRemoteLinux(t *testing.T) {
	payload, err := protojson.Marshal(&agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{
			OsVersion:      "linux 6.12.41+deb13-amd64",
			Shell:          "/bin/bash",
			WorkspacePaths: []string{"/root/teapot"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation := &ConversationFile{Entries: []HistoryEntry{{
		Kind:    "request_context",
		Payload: payload,
	}}}
	got := cursorAccountForwardContext(conversation, []string{`C:\Users\admin\Documents\cursor-byok`}, `C:\terminals`)
	env := got.GetEnv()
	if env.GetOsVersion() != "linux 6.12.41+deb13-amd64" || env.GetShell() != "/bin/bash" {
		t.Fatalf("env os=%q shell=%q", env.GetOsVersion(), env.GetShell())
	}
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != "/root/teapot" {
		t.Fatalf("workspace = %v", paths)
	}
	if strings.Contains(env.GetTerminalsFolder(), `C:\`) {
		t.Fatalf("local terminals folder replaced the remote context: %q", env.GetTerminalsFolder())
	}
}

func TestCursorAccountForwardContextUsesStreamWorkspace(t *testing.T) {
	got := cursorAccountForwardContext(nil, []string{"/srv/teapot"}, "/srv/teapot/terminals")
	env := got.GetEnv()
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != "/srv/teapot" {
		t.Fatalf("workspace = %v", paths)
	}
	if env.GetProjectFolder() != "/srv/teapot" || env.GetTerminalsFolder() != "/srv/teapot/terminals" {
		t.Fatalf("project=%q terminals=%q", env.GetProjectFolder(), env.GetTerminalsFolder())
	}
}

func TestCursorAccountForwardContextPreservesTurn1EnvAcrossRealtimeTurn2(t *testing.T) {
	turn1Payload, err := protojson.Marshal(&agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{
			OsVersion:       "win32 10.0.26200",
			Shell:           "powershell",
			WorkspacePaths:  []string{`c:\Users\admin\Documents\cursor-byok`},
			TerminalsFolder: `C:\Users\admin\.cursor\projects\cursor-byok\terminals`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	commitAttr := "enabled"
	turn2RealtimePayload, err := protojson.Marshal(&agentv1.RequestContext{
		CommitAttributionMessage: &commitAttr,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation := &ConversationFile{Entries: []HistoryEntry{
		{TurnSeq: 1, Kind: "request_context", Payload: turn1Payload},
		{TurnSeq: 2, Kind: "request_context", Payload: turn2RealtimePayload},
	}}
	got := cursorAccountForwardContext(conversation, nil, "")
	env := got.GetEnv()
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != `c:\Users\admin\Documents\cursor-byok` {
		t.Fatalf("follow-up lost Turn 1 workspace paths: %v", paths)
	}
	if got.GetCommitAttributionMessage() != "enabled" {
		t.Fatalf("follow-up lost Turn 2 realtime fields: %q", got.GetCommitAttributionMessage())
	}
}
