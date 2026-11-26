package qrcode

import (
	"fmt"
	"strings"
	"testing"
)

func TestQRCodePreservesFourModuleQuietZone(t *testing.T) {
	term := New()
	qr := term.Get("test-qr-payload")
	if qr == nil {
		t.Fatalf("expected non-nil QRCodeString")
	}

	lines := strings.Split(strings.TrimSuffix(string(*qr), "\n"), "\n")
	if len(lines) < 29 {
		t.Fatalf("expected at least 29 rows including 4-module quiet zone, got %d", len(lines))
	}

	frontToken := fmt.Sprint(term.front)
	for i := 0; i < 4; i++ {
		if strings.Contains(lines[i], frontToken) {
			t.Fatalf("top quiet zone row %d contains foreground module", i)
		}
		if strings.Contains(lines[len(lines)-1-i], frontToken) {
			t.Fatalf("bottom quiet zone row %d contains foreground module", len(lines)-1-i)
		}
	}
}
