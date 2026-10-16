package main

import (
	"testing"
)

func TestExecutorSSEFramingIsOwnedByCPA(t *testing.T) {
	frame, err := executorFrame([]byte("data: {\"object\":\"chat.completion.chunk\"}\n\n"))
	if err != nil || string(frame) != `{"object":"chat.completion.chunk"}` {
		t.Fatal(string(frame), err)
	}
	for _, input := range []string{": keepalive\n\n", "data: [DONE]\n\n"} {
		frame, err := executorFrame([]byte(input))
		if err != nil || len(frame) != 0 {
			t.Fatal("control frame forwarded as JSON", input)
		}
	}
}
