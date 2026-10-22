package agent

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// discussImageStore is the media store of a discuss run: one distinct picture
// per content hash, remembering which of them the run opened.
type discussImageStore struct {
	mu     sync.Mutex
	pixels map[string][]byte
	opened []string
}

func newDiscussImageStore(t *testing.T) *discussImageStore {
	t.Helper()
	return &discussImageStore{pixels: map[string][]byte{}}
}

// put stores a picture for hash and returns the asset link a row carries.
func (s *discussImageStore) put(t *testing.T, hash string) messagepkg.AssetRef {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	// Each picture is noise from its own seed, so none equals another.
	state := uint32(0x9E3779B9)
	for range s.pixels {
		state *= 1664525
	}
	for y := range 64 {
		for x := range 64 {
			state = state*1664525 + 1013904223
			//nolint:gosec // G115: deliberate byte extraction from the noise state.
			img.Set(x, y, color.RGBA{R: uint8(state >> 24), G: uint8(state >> 16), B: uint8(state >> 8), A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	s.pixels[hash] = buf.Bytes()
	return messagepkg.AssetRef{ContentHash: hash, Role: "attachment", Mime: "image/png", SizeBytes: int64(buf.Len())}
}

func (s *discussImageStore) loader() *agenttest.AssetLoader {
	return &agenttest.AssetLoader{
		OpenFn: func(_ context.Context, _, hash string) (io.ReadCloser, string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.opened = append(s.opened, hash)
			data, ok := s.pixels[hash]
			if !ok {
				return nil, "", fmt.Errorf("no asset %s", hash)
			}
			return io.NopCloser(bytes.NewReader(data)), "image/png", nil
		},
	}
}

func (s *discussImageStore) opens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.opened...)
}

// dataURL is what the model receives for the picture stored under hash.
func (s *discussImageStore) dataURL(hash string) string {
	return agenttest.DataURL("image/png", s.pixels[hash])
}

// imagesByMessage maps the text of each user message of a provider request to
// the images that message carries, as the stored hashes they are.
func (s *discussImageStore) imagesByMessage(req sdk.Request) map[string][]string {
	byURL := map[string]string{}
	for hash := range s.pixels {
		byURL[s.dataURL(hash)] = hash
	}
	out := map[string][]string{}
	for _, message := range req.Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		var text string
		var images []string
		for _, part := range message.Content {
			switch part := part.(type) {
			case sdk.TextPart:
				if text == "" {
					text = part.Text
				}
			case sdk.ImagePart:
				images = append(images, byURL[part.Image])
			}
		}
		if len(images) > 0 {
			out[text] = append(out[text], images...)
		}
	}
	return out
}

func joined(hashes []string) string { return strings.Join(hashes, ",") }

// runDiscussImages writes the rows, runs the discuss run that answers them with
// a vision-capable model and the given media store, and returns the request the
// model received together with the configuration the run was built with.
func runDiscussImages(t *testing.T, env discussEnv, store *discussImageStore, vision bool, write func()) (sdk.Request, native.RunConfig) {
	t.Helper()
	var (
		mu  sync.Mutex
		cfg native.RunConfig
	)
	options := []discussOption{
		discussDeps(func(d *Deps) { d.Assets = store.loader() }),
		discussObserveConfig(func(c native.RunConfig) {
			mu.Lock()
			defer mu.Unlock()
			cfg = c
		}),
	}
	if vision {
		options = append(options, discussVision())
	}
	proc := newDiscussProcessWith(t, env, options, &discussStep{})
	write()
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	requests := proc.provider.recorded()
	if len(requests) == 0 {
		t.Fatal("the run did not reach the model provider")
	}
	mu.Lock()
	defer mu.Unlock()
	return requests[0], cfg
}

func omittedSources(cfg native.RunConfig) []string {
	var out []string
	for _, record := range cfg.ContextMutations.Records() {
		if record.Kind == contextfrag.MutationCurrentInputImagesOmitted {
			out = append(out, record.Detail)
		}
	}
	return out
}

// Each image of the batch rides on the message it arrived with, and the images
// of the row the run answers ride on that row. Rows the run does not answer, or
// that never reached its context, lose their images with them: the ones an
// earlier run covered, and the ones refused at admission.
func TestDiscussInlinesImagesOnTheirOwnMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	store := newDiscussImageStore(t)
	alice, bob := env.participant(t, false), env.participant(t, false)
	first := &discussStep{}
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussVision(),
		discussDeps(func(d *Deps) { d.Assets = store.loader() }),
	}, first)

	// The first round is answered with its own picture; the second round must
	// not show it again.
	env.write(t, passiveInput{from: alice, externalID: "old", text: "old photo", assets: []messagepkg.AssetRef{store.put(t, "h-old")}})
	hw := env.write(t, passiveInput{from: alice, externalID: "ask", text: "ask"})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("first outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(hw), "completed")
	if got := store.opens(); joined(got) != "h-old" {
		t.Fatalf("first round opened %v, want only its own picture", got)
	}

	hw2 := env.writeAll(t,
		passiveInput{from: alice, externalID: "m1", text: "m1 two pictures", assets: []messagepkg.AssetRef{store.put(t, "h-m1a"), store.put(t, "h-m1b")}},
		passiveInput{from: bob, externalID: "refused", text: "refused", refused: messagepkg.Admission("refused"), assets: []messagepkg.AssetRef{store.put(t, "h-refused")}},
		passiveInput{from: bob, externalID: "m2", text: "m2 text only"},
		passiveInput{from: bob, externalID: "m3", text: "m3 newest", assets: []messagepkg.AssetRef{store.put(t, "h-m3")}},
	)[3]
	store.mu.Lock()
	store.opened = nil
	store.mu.Unlock()
	proc.answer(t, env, hw2)

	requests := proc.provider.recorded()
	got := store.imagesByMessage(requests[len(requests)-1])
	if len(got) != 2 || joined(got["m1 two pictures"]) != "h-m1a,h-m1b" || joined(got["m3 newest"]) != "h-m3" {
		t.Fatalf("images by message = %v, want each picture on its own message", got)
	}
	if opened := store.opens(); len(opened) != 3 {
		t.Fatalf("second round opened %v, want only the batch's three pictures", opened)
	}
}

// A model without vision gets no image and nothing is read from the media
// store for it.
func TestDiscussLoadsNoImagesWithoutVision(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	store := newDiscussImageStore(t)
	alice := env.participant(t, false)
	request, cfg := runDiscussImages(t, env, store, false, func() {
		env.write(t, passiveInput{from: alice, externalID: "m1", text: "m1", assets: []messagepkg.AssetRef{store.put(t, "h1")}})
		env.write(t, passiveInput{from: alice, externalID: "m2", text: "m2", assets: []messagepkg.AssetRef{store.put(t, "h2")}})
	})
	if got := store.imagesByMessage(request); len(got) != 0 {
		t.Fatalf("images by message = %v, want none", got)
	}
	if opened := store.opens(); len(opened) != 0 {
		t.Fatalf("opened %v, want nothing read", opened)
	}
	if omitted := omittedSources(cfg); len(omitted) != 0 {
		t.Fatalf("mutations %v, want none recorded for a model that never gets images", omitted)
	}
}

// The turn-wide vision budget goes to the newest input first; the picture it
// has no room for is recorded as left out.
func TestDiscussVisionBudgetKeepsTheNewestInputImages(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	store := newDiscussImageStore(t)
	alice := env.participant(t, false)
	const rows = 21
	request, cfg := runDiscussImages(t, env, store, true, func() {
		for i := range rows {
			id := fmt.Sprintf("m%02d", i)
			env.write(t, passiveInput{from: alice, externalID: id, text: id, assets: []messagepkg.AssetRef{store.put(t, "h-"+id)}})
		}
	})
	got := store.imagesByMessage(request)
	if len(got) != rows-1 || got["m00"] != nil || joined(got["m20"]) != "h-m20" || joined(got["m01"]) != "h-m01" {
		t.Fatalf("images on %d messages (m00=%v), want the newest %d kept", len(got), got["m00"], rows-1)
	}
	omitted := omittedSources(cfg)
	if len(omitted) != 1 || !strings.HasPrefix(omitted[0], "sources=") || strings.Contains(omitted[0], ",") {
		t.Fatalf("mutations = %v, want exactly the oldest row recorded as left out", omitted)
	}
}

// A picture sent twice belongs to its newest sender: the older message is
// history a budget may trim, the newest input is protected.
func TestDiscussRepeatedImageRidesOnTheNewestMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	store := newDiscussImageStore(t)
	alice, bob := env.participant(t, false), env.participant(t, false)
	sticker := store.put(t, "sticker")
	request, _ := runDiscussImages(t, env, store, true, func() {
		env.write(t, passiveInput{from: alice, externalID: "alice", text: "alice", assets: []messagepkg.AssetRef{sticker}})
		env.write(t, passiveInput{from: bob, externalID: "bob", text: "bob", assets: []messagepkg.AssetRef{sticker}})
	})
	got := store.imagesByMessage(request)
	if len(got) != 1 || joined(got["bob"]) != "sticker" {
		t.Fatalf("images by message = %v, want the picture only on the newest sender", got)
	}
	if opened := store.opens(); len(opened) != 1 {
		t.Fatalf("opened %v, want the repeated picture read once", opened)
	}
}

// discussVision makes the run's model accept image input.
func discussVision() discussOption {
	return func(s *discussSetup) { s.compatibilities = append(s.compatibilities, "vision") }
}
