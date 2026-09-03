package proxy

import "testing"

func TestSSEDecoderRecognizesCompletedResponse(t *testing.T) {
	decoder := newSSEDecoder(1024)
	event, err := decoder.Feed([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != terminalSuccess {
		t.Fatalf("kind = %v, want terminalSuccess", event.Kind)
	}
}

func TestSSEDecoderRecognizesAnthropicMessageStop(t *testing.T) {
	decoder := newSSEDecoder(1024)
	event, err := decoder.Feed([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != terminalSuccess {
		t.Fatalf("kind = %v, want terminalSuccess", event.Kind)
	}
}

func TestSSEDecoderDoesNotTreatLegacyDoneAsSuccess(t *testing.T) {
	decoder := newSSEDecoder(1024)
	event, err := decoder.Feed([]byte("data: [DONE]\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind == terminalSuccess {
		t.Fatal("legacy [DONE] marker must not complete a native stream")
	}
}

func TestSSEDecoderRecognizesCapacityFailureMessage(t *testing.T) {
	decoder := newSSEDecoder(4096)
	event, err := decoder.Feed([]byte(
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"}}}\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != terminalFailure {
		t.Fatalf("kind = %v, want terminalFailure", event.Kind)
	}
	if event.Message != "Selected model is at capacity. Please try a different model." {
		t.Fatalf("message = %q", event.Message)
	}
}

func TestSSEDecoderRecognizesErrorEvent(t *testing.T) {
	decoder := newSSEDecoder(1024)
	event, err := decoder.Feed([]byte("event: error\ndata: {\"error\":{\"message\":\"upstream failed\"}}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != terminalFailure {
		t.Fatalf("kind = %v, want terminalFailure", event.Kind)
	}
}
