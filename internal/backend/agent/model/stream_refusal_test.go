package modeladapter

import (
	"errors"
	"io"
	"strings"
	"testing"

	"cursor/internal/apperror"
)

// Cursor can refuse a call after part of the response has gone out. The
// refusal ends the stream and carries the server's own words for what the
// user has to do. That is not a lost connection. Reported as one, the
// explanation sits behind two sentences about an interrupted stream, and the
// user retries a turn that will be refused again.

const unpaidInvoiceRefusal = "cursor refused the model call: Pay your unpaid invoice at [cursor.com/dashboard/billing](https://cursor.com/dashboard/billing) to resume requests."

func TestRefusalAfterPartialOutputReadsAsTheRefusal(t *testing.T) {
	refusal := &cursorAccountRefusalError{message: unpaidInvoiceRefusal}

	err := midStreamInterruptedError(refusal)

	if got := err.Error(); got != unpaidInvoiceRefusal {
		t.Fatalf("message = %q, want the refusal as Cursor worded it", got)
	}
	if !errors.Is(err, ErrMidStreamInterrupted) {
		t.Fatal("the call already produced output and must stay marked as unsafe to send again")
	}
	var kept *cursorAccountRefusalError
	if !errors.As(err, &kept) || kept != refusal {
		t.Fatal("the refusal and its dashboard actions must stay reachable")
	}
}

// What the router hands the forwarder is what the IDE shows.
func TestRefusalAfterPartialOutputReachesTheUserWithoutTheInterruptionWording(t *testing.T) {
	failure := classifyRouterError(StreamRequest{RequestID: "req"}, midStreamInterruptedError(&cursorAccountRefusalError{message: unpaidInvoiceRefusal}))

	shown := failure.Error()
	if !strings.HasPrefix(shown, "cursor refused the model call: Pay your unpaid invoice") {
		t.Fatalf("message shown = %q, want it to open with the refusal", shown)
	}
	if strings.Contains(shown, "interrupted") || strings.Contains(shown, "reconnect") {
		t.Fatalf("message shown = %q: nothing was interrupted, Cursor declined the call", shown)
	}
	var classified *apperror.AppError
	if !errors.As(failure, &classified) {
		t.Fatalf("failure is %T, want a classified error", failure)
	}
	if classified.Code == apperror.CodeStreamInterrupted {
		t.Fatal("classified as an interrupted stream, whose advice is to retry the turn; a refusal is refused again")
	}
	if classified.Disposition != apperror.DispositionFatal {
		t.Fatalf("disposition = %q, want fatal: output already went out, the call must not be sent again", classified.Disposition)
	}
}

func TestLostConnectionAfterPartialOutputIsClassifiedAsAnInterruptedStream(t *testing.T) {
	failure := classifyRouterError(StreamRequest{RequestID: "req"}, midStreamInterruptedError(io.ErrUnexpectedEOF))

	var classified *apperror.AppError
	if !errors.As(failure, &classified) || classified.Code != apperror.CodeStreamInterrupted || classified.Disposition != apperror.DispositionFatal {
		t.Fatalf("classification = %+v, want an interrupted stream that is not retried", classified)
	}
}

func TestLostConnectionAfterPartialOutputStillSaysTheStreamWasInterrupted(t *testing.T) {
	err := midStreamInterruptedError(io.ErrUnexpectedEOF)

	if !strings.Contains(err.Error(), "upstream stream interrupted mid-response") {
		t.Fatalf("message = %q, want the interruption wording for a lost connection", err.Error())
	}
	if !errors.Is(err, ErrMidStreamInterrupted) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("the marker and the cause must both stay reachable")
	}
}
