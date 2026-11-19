package runtimecore

import (
	"testing"

	"cursor/gen/agentv1"
)

func TestDecodeCreatePlanArgsJSONKeepsProtoJSONProjectFlag(t *testing.T) {
	args, err := DecodeCreatePlanArgsJSON([]byte(`{"name":"mode switch","plan":"trace the decoder","isProject":true,"todos":[{"id":"1","content":"decode","status":1,"createdAt":4}]}`))
	if err != nil {
		t.Fatalf("DecodeCreatePlanArgsJSON() error = %v", err)
	}
	if !args.GetIsProject() {
		t.Fatal("isProject was dropped")
	}
	if len(args.GetTodos()) != 1 || args.GetTodos()[0].GetCreatedAt() != 4 || args.GetTodos()[0].GetStatus() != agentv1.TodoStatus_TODO_STATUS_PENDING {
		t.Fatalf("todos = %#v", args.GetTodos())
	}
}

func TestDecodeCreatePlanArgsJSONKeepsStringTodoStatus(t *testing.T) {
	args, err := DecodeCreatePlanArgsJSON([]byte(`{"plan":"keep going","is_project":true,"todos":[{"id":"1","content":"decode","status":"pending","created_at":4}]}`))
	if err != nil {
		t.Fatalf("DecodeCreatePlanArgsJSON() error = %v", err)
	}
	if !args.GetIsProject() {
		t.Fatal("is_project was dropped")
	}
	if len(args.GetTodos()) != 1 || args.GetTodos()[0].GetStatus() != agentv1.TodoStatus_TODO_STATUS_PENDING {
		t.Fatalf("todos = %#v", args.GetTodos())
	}
	if args.GetTodos()[0].GetCreatedAt() != 4 {
		t.Fatalf("created_at = %d", args.GetTodos()[0].GetCreatedAt())
	}
}
