package main

import "testing"

func TestSSEParser(t *testing.T) {
	var p sseParser
	lines := []string{
		"retry: 3000", // advisory field: consumed, no frame
		"",
		": ping", // keepalive comment
		"",
		"event: connected",
		`data: {"session":"s1"}`,
		"",
		"event: message",
		`data: {"a":`,
		`data: 2}`, // multi-line data joins with \n per the SSE spec
		"",
		"event: message\r",
		"data: {\"crlf\":true}\r", // CRLF line endings
		"data:no-space",           // space after colon is optional
		"",
	}

	var frames []sseFrame
	for _, l := range lines {
		if f, ok := p.feed(l); ok {
			frames = append(frames, f)
		}
	}

	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].Event != "connected" || frames[0].Data != `{"session":"s1"}` {
		t.Fatalf("frame 0 wrong: %+v", frames[0])
	}
	if frames[1].Event != "message" || frames[1].Data != "{\"a\":\n2}" {
		t.Fatalf("frame 1 wrong: %+v", frames[1])
	}
	if frames[2].Event != "message" || frames[2].Data != "{\"crlf\":true}\nno-space" {
		t.Fatalf("frame 2 wrong: %q / %q", frames[2].Event, frames[2].Data)
	}
}
