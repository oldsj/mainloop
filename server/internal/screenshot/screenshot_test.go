package screenshot_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/oldsj/mainloop/server/internal/screenshot"
)

func TestViewportNames(t *testing.T) {
	for name, want := range map[string]screenshot.Viewport{"mobile": screenshot.Mobile, "desktop": screenshot.Desktop} {
		if got, ok := screenshot.ViewportNamed(name); !ok || got != want {
			t.Errorf("ViewportNamed(%q) = %v, %v", name, got, ok)
		}
	}
	if _, ok := screenshot.ViewportNamed("tablet"); ok {
		t.Error("an unknown viewport was accepted")
	}
	if !screenshot.Mobile.Mobile || screenshot.Desktop.Mobile {
		t.Error("only the mobile viewport emulates a phone")
	}
}

func TestCaptureNeedsABrowserAndACookie(t *testing.T) {
	ctx := context.Background()
	if _, err := (&screenshot.CDP{}).Capture(ctx, screenshot.Shot{URL: "https://x.test/", Cookie: &http.Cookie{Name: "c", Value: "v"}}); err == nil {
		t.Error("captured without a browser")
	}
	if _, err := (&screenshot.CDP{URL: "http://127.0.0.1:9"}).Capture(ctx, screenshot.Shot{URL: "https://x.test/"}); err == nil {
		t.Error("captured without a preview cookie")
	}
}
