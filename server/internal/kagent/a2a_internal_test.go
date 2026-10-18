package kagent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// TestBuildMessageCarriesARealImageAsAMultimodalPart builds the A2A message for a turn with a
// pasted image, using a real PNG fixture (not a synthetic byte slice), and checks that the wire
// form carries the exact image bytes, base64-encoded, under the media type the upload validated.
// This is the multimodal content block this repo hands to kagent for the claude profile; kagent
// (a separate harness, not this repo) turns it into the provider's own request shape.
func TestBuildMessageCarriesARealImageAsAMultimodalPart(t *testing.T) {
	png, err := os.ReadFile(filepath.Join("testdata", "pixel.png"))
	if err != nil {
		t.Fatal(err)
	}
	msg := buildMessage(SendParams{
		Text: "what is this?", MessageID: "m1", ContextID: "c1",
		Images: []ImageAttachment{{Data: png, MediaType: "image/png"}},
	})
	if len(msg.Parts) != 2 {
		t.Fatalf("%d parts", len(msg.Parts))
	}
	if msg.Parts[0].Text() != "what is this?" {
		t.Fatalf("text part %+v", msg.Parts[0])
	}
	imagePart := msg.Parts[1]
	if imagePart.MediaType != "image/png" {
		t.Fatalf("media type %q", imagePart.MediaType)
	}
	if got := imagePart.Raw(); string(got) != string(png) {
		t.Fatalf("raw bytes do not round-trip: got %d bytes, want %d", len(got), len(png))
	}

	// The wire form the A2A client actually sends: raw bytes flatten to a base64 string next to
	// their media type, the same shape kagent decodes on the other end.
	wire, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Parts []struct {
			Raw       string `json:"raw"`
			MediaType string `json:"mediaType"`
			Text      string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Parts) != 2 {
		t.Fatalf("%d wire parts", len(decoded.Parts))
	}
	if decoded.Parts[1].MediaType != "image/png" {
		t.Fatalf("wire media type %q", decoded.Parts[1].MediaType)
	}
	wantB64 := base64.StdEncoding.EncodeToString(png)
	if decoded.Parts[1].Raw != wantB64 {
		t.Fatalf("wire base64 does not match the fixture's real encoding")
	}
}

// TestBuildMessageTextOnlyHasNoImagePart keeps the plain, text-only shape unchanged: no Images
// means no second part.
func TestBuildMessageTextOnlyHasNoImagePart(t *testing.T) {
	msg := buildMessage(SendParams{Text: "hello", MessageID: "m1", ContextID: "c1"})
	if len(msg.Parts) != 1 || msg.Parts[0].Text() != "hello" {
		t.Fatalf("parts %+v", msg.Parts)
	}
}

// TestBuildMessageImageOnlyHasNoTextPart matches the paste-only composer flow: an image with no
// caption sends no empty text part.
func TestBuildMessageImageOnlyHasNoTextPart(t *testing.T) {
	msg := buildMessage(SendParams{
		MessageID: "m1", ContextID: "c1",
		Images: []ImageAttachment{{Data: []byte("fake-bytes"), MediaType: "image/jpeg"}},
	})
	if len(msg.Parts) != 1 {
		t.Fatalf("%d parts", len(msg.Parts))
	}
	if _, ok := msg.Parts[0].Content.(a2a.Text); ok {
		t.Fatalf("unexpected text part %+v", msg.Parts[0])
	}
}
