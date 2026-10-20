package sessions_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// pixelPNG is a real, small PNG fixture (the same one the kagent package tests use for the
// multimodal content-block construction), not synthetic bytes.
func pixelPNG(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "kagent", "testdata", "pixel.png"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUploadImageValidatesFormatAndSize(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	png := pixelPNG(t)

	if _, err := e.svc.UploadImage(ctx, "image/svg+xml", png); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("unsupported format: got %v", err)
	}
	if _, err := e.svc.UploadImage(ctx, "image/png", nil); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("empty data: got %v", err)
	}
	oversized := make([]byte, sessions.MaxImageBytes+1)
	if _, err := e.svc.UploadImage(ctx, "image/png", oversized); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("oversized: got %v", err)
	}

	ref, err := e.svc.UploadImage(ctx, "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID == "" || ref.MediaType != "image/png" || ref.SizeBytes != len(png) {
		t.Fatalf("ref %+v", ref)
	}
}

func TestSendWithImagesAttachesTheUploadToTheMessage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	png := pixelPNG(t)

	ref, err := e.svc.UploadImage(ctx, "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	sent, err := e.svc.SendWithImages(ctx, id, "", "what is this?", []string{ref.ID})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := dbq.New(e.pool).ListMessageImagesByMessage(ctx, sent.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != ref.ID || rows[0].MediaType != "image/png" || string(rows[0].Data) != string(png) {
		t.Fatalf("attached images %+v", rows)
	}
}

func TestSendWithImagesAllowsEmptyTextWhenAnImageIsGiven(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ref, err := e.svc.UploadImage(ctx, "image/jpeg", pixelPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SendWithImages(ctx, uuid.NewString(), "", "", []string{ref.ID}); err != nil {
		t.Fatalf("image with no caption: %v", err)
	}
	// Neither text nor an image is still refused.
	if _, err := e.svc.SendWithImages(ctx, uuid.NewString(), "", "   ", nil); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("empty send: got %v", err)
	}
}

func TestSendWithImagesRejectsAnUnknownOrAlreadyClaimedImageID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()

	if _, err := e.svc.SendWithImages(ctx, uuid.NewString(), "", "hi", []string{"no-such-image"}); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("unknown id: got %v", err)
	}

	ref, err := e.svc.UploadImage(ctx, "image/gif", pixelPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SendWithImages(ctx, uuid.NewString(), "", "first", []string{ref.ID}); err != nil {
		t.Fatal(err)
	}
	// The same image cannot be claimed by a second, different message.
	if _, err := e.svc.SendWithImages(ctx, uuid.NewString(), "", "second", []string{ref.ID}); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("re-claim: got %v", err)
	}
}

func TestSendWithImagesRejectsTooManyImages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ids := make([]string, sessions.MaxImagesPerMessage+1)
	for i := range ids {
		ref, err := e.svc.UploadImage(ctx, "image/png", pixelPNG(t))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = ref.ID
	}
	if _, err := e.svc.SendWithImages(ctx, uuid.NewString(), "", "many", ids); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("too many images: got %v", err)
	}
}

// TestAnImageOnlyMessageIsDeliveredSuccessfully runs the real delivery loop against the fake
// kagent server: a message with no text but a pasted image must not be settled as failed with
// "the message text is missing", now that an image alone is enough to send a turn.
func TestAnImageOnlyMessageIsDeliveredSuccessfully(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	ctx := context.Background()
	ref, err := e.svc.UploadImage(ctx, "image/png", pixelPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err := e.svc.SendWithImages(ctx, id, "", "", []string{ref.ID}); err != nil {
		t.Fatal(err)
	}
	d := e.waitState(id, dbstate.DeliveryCompleted)
	if deref(d.Detail) != "" {
		t.Fatalf("delivery detail %q", deref(d.Detail))
	}
}

func TestSendWithImagesIsIdempotentPerMessageID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ref, err := e.svc.UploadImage(ctx, "image/png", pixelPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	first, err := e.svc.SendWithImages(ctx, id, "", "hi", []string{ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.SendWithImages(ctx, id, first.ConversationID, "hi", []string{ref.ID})
	if err != nil || again != first {
		t.Fatalf("second send %+v, %v; first %+v", again, err, first)
	}
	rows, err := dbq.New(e.pool).ListMessageImagesByMessage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d images after a repeated send", len(rows))
	}
}

// TestSendToValidatesTextTheSameWayAfterTheSharedRefactor checks that SendTo/SendToAs (a worker
// session's send, which takes no image parameter at all) still refuses an empty or over-long
// message the same way: checkText became checkTextOrImages shared with the main chat, and this
// guards that the worker path's behavior did not move. Text validation runs before any session
// lookup, so a session id that does not exist is enough to exercise it.
// TestUploadThenSendOverHTTPShapeShowsTheImageInTheConversation drives the two endpoints as the
// web client does: upload the image, then send a message naming its id, then read the
// conversation back and find the same image, with its real bytes, on the stored message. A
// worker session's conversation (GetSessionConversation) is untouched by this feature and is not
// exercised here.
func TestUploadThenSendOverHTTPShapeShowsTheImageInTheConversation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.API()
	ctx := context.Background()
	png := pixelPNG(t)

	uploaded, err := a.UploadChatImage(ctx, api.UploadChatImageRequestObject{
		Body: &api.ChatImageUpload{MediaType: "image/png", Data: png},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := api.ChatImageUploadResponse(uploaded.(api.UploadChatImage200JSONResponse))
	if up.Id == "" || up.MediaType != "image/png" || up.SizeBytes != len(png) {
		t.Fatalf("upload response %+v", up)
	}

	sent, err := a.SendMessage(ctx, api.SendMessageRequestObject{
		Body: &api.ChatRequest{Message: "what is this?", ImageIds: &[]string{up.Id}},
	})
	if err != nil {
		t.Fatal(err)
	}
	chatResp := api.ChatResponse(sent.(api.SendMessage200JSONResponse))

	conv, err := a.GetConversation(ctx, api.GetConversationRequestObject{ConversationId: chatResp.ConversationId})
	if err != nil {
		t.Fatal(err)
	}
	c := api.ConversationResponse(conv.(api.GetConversation200JSONResponse))
	if len(c.Messages) != 1 {
		t.Fatalf("%d messages", len(c.Messages))
	}
	msg := c.Messages[0]
	if msg.Content != "what is this?" || msg.Images == nil || len(*msg.Images) != 1 {
		t.Fatalf("message %+v", msg)
	}
	img := (*msg.Images)[0]
	if img.Id != up.Id || img.MediaType != "image/png" || string(img.Data) != string(png) {
		t.Fatalf("message image %+v", img)
	}
}

// TestUploadChatImageRejectsAnUnsupportedFormatOverHTTPShape checks the handler maps the
// service's format rejection to a 422, the contract's validation-error status.
func TestUploadChatImageRejectsAnUnsupportedFormatOverHTTPShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.API()
	_, err := a.UploadChatImage(context.Background(), api.UploadChatImageRequestObject{
		Body: &api.ChatImageUpload{MediaType: "image/svg+xml", Data: pixelPNG(t)},
	})
	if apierr.StatusOf(err) != 422 {
		t.Fatalf("got %v", err)
	}
}

func TestSendToValidatesTextTheSameWayAfterTheSharedRefactor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.SendTo(ctx, "no-such-session", uuid.NewString(), "   "); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("empty send to a session: got %v", err)
	}
	if _, err := e.svc.SendTo(ctx, "no-such-session", uuid.NewString(), strings.Repeat("x", sessions.MaxMessageRunes+1)); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("too long: got %v", err)
	}
}
