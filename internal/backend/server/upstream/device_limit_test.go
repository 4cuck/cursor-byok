package upstream

import (
	"bytes"
	"strings"
	"testing"
)

// The sniffer must detect the device-cap refusal without buffering the whole
// response, and must not fire on ordinary streaming output.
func TestDeviceLimitSnifferDetectsLockMessage(t *testing.T) {
	fired := 0
	sniffer := &deviceLimitSniffer{onDetect: func() { fired++ }}

	// Frames arrive split across reads, as they would on the wire.
	sniffer.inspect([]byte("\x02\x00\x00\x01\x40{\"error\":{\"code\":\"resource_exhausted\""))
	sniffer.inspect([]byte(`,"message":"Error","details":[{"debug":{"error":"ERROR_CUSTOM_MESSAGE"`))
	sniffer.inspect([]byte(`,"details":{"title":"Too many computers.","detail":"Too many computers `))
	sniffer.inspect([]byte(`used within the last 24 hours for the same Cursor account."}}}]}}`))

	if fired != 1 {
		t.Fatalf("onDetect fired %d times, want exactly 1", fired)
	}
	if !sniffer.scanned {
		t.Fatal("sniffer should stop scanning after a match")
	}
}

func TestDeviceLimitSnifferIgnoresNormalStreams(t *testing.T) {
	fired := 0
	sniffer := &deviceLimitSniffer{onDetect: func() { fired++ }}

	normal := `{"streamUnifiedChatResponse":{"text":"Hello! How can I help you today?"}}`
	for index := 0; index < 200; index++ {
		sniffer.inspect([]byte(normal))
	}
	if fired != 0 {
		t.Fatalf("onDetect fired %d times on a normal stream", fired)
	}
}

// copyResponseWithSniffer must forward every byte unchanged while still firing.
func TestCopyResponseWithSnifferPreservesStream(t *testing.T) {
	payload := strings.Repeat("data-frame-content;", 100) + `"title":"Too many computers."` + strings.Repeat("tail;", 100)

	fired := 0
	var out bytes.Buffer
	written, err := copyResponseWithSniffer(&out, strings.NewReader(payload), func() { fired++ })
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if int(written) != len(payload) {
		t.Fatalf("wrote %d bytes, want %d", written, len(payload))
	}
	if out.String() != payload {
		t.Fatal("stream content was altered")
	}
	if fired != 1 {
		t.Fatalf("onDetect fired %d times, want 1", fired)
	}
}

// A nil callback must be safe: forwarding happens on every request.
func TestCopyResponseWithSnifferNilCallback(t *testing.T) {
	payload := "plain body"
	var out bytes.Buffer
	if _, err := copyResponseWithSniffer(&out, strings.NewReader(payload), nil); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if out.String() != payload {
		t.Fatal("body altered")
	}
}