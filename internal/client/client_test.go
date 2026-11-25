package client

import (
	"bytes"
	"strings"
	"testing"

	"a310ctl/internal/controller"
	"a310ctl/internal/gpu"
)

func TestPrintUnavailable(t *testing.T) {
	var out bytes.Buffer
	PrintStatus(&out, controller.Status{Address: "0000:03:00.0", Errors: []string{"card unavailable"}})
	if text := out.String(); !strings.Contains(text, "not responding") || strings.Contains(text, "power sustained") {
		t.Fatalf("status of an unavailable card:\n%s", text)
	}
	out.Reset()
	PrintSensors(&out, gpu.Sensors{})
	if text := out.String(); !strings.Contains(text, "not responding") || strings.Contains(text, "gpu temperature") {
		t.Fatalf("sensors of an unavailable card:\n%s", text)
	}
}
