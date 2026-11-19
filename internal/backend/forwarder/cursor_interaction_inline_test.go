package forwarder

import (
	"testing"

	"cursor/gen/agentv1"
)

func TestDeliverCursorInteractionResultMarksInlineTurn(t *testing.T) {
	stream := &ActiveStream{
		CursorInteractionReplies: make(chan *agentv1.InteractionResponse, 1),
	}
	if !deliverCursorInteractionResult(stream, &agentv1.InteractionResponse{Id: 7}) {
		t.Fatal("deliverCursorInteractionResult() = false, want true")
	}
	stream.mu.Lock()
	inline := stream.CursorInlineToolTurn
	stream.mu.Unlock()
	if !inline {
		t.Fatal("CursorInlineToolTurn = false, want true after an interaction reply is written to the open stream")
	}
}

func TestDeliverCursorInteractionResultWithoutChannelDoesNotMarkInlineTurn(t *testing.T) {
	stream := &ActiveStream{}
	if deliverCursorInteractionResult(stream, &agentv1.InteractionResponse{Id: 7}) {
		t.Fatal("deliverCursorInteractionResult() = true, want false")
	}
	if stream.CursorInlineToolTurn {
		t.Fatal("CursorInlineToolTurn = true, want false when the reply was not delivered")
	}
}
