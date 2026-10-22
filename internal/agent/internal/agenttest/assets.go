package agenttest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"
)

// AssetLoader is a gateway asset loader answered by functions; a nil function
// answers io.EOF.
type AssetLoader struct {
	OpenFn       func(ctx context.Context, botID, contentHash string) (io.ReadCloser, string, error)
	AccessPathFn func(ctx context.Context, botID, contentHash string) (string, error)
}

func (f *AssetLoader) AccessPathForGateway(ctx context.Context, botID, contentHash string) (string, error) {
	if f == nil || f.AccessPathFn == nil {
		return "", io.EOF
	}
	return f.AccessPathFn(ctx, botID, contentHash)
}

func (f *AssetLoader) OpenForGateway(ctx context.Context, botID, contentHash string) (io.ReadCloser, string, error) {
	if f == nil || f.OpenFn == nil {
		return nil, "", io.EOF
	}
	return f.OpenFn(ctx, botID, contentHash)
}

// RasterPNG returns real PNG bytes. Image input is now decided by the bytes, so
// fixtures have to be actual images rather than a label and a placeholder.
func RasterPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	// Noise, not a gradient: the fixture has to survive PNG compression so it
	// stays larger than the sniff window the validator reads.
	state := uint32(0x9E3779B9)
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
	if buf.Len() <= 512 {
		t.Fatalf("fixture must exceed the 512-byte sniff window, got %d bytes", buf.Len())
	}
	return buf.Bytes()
}

// WebMSticker mimics a Telegram video sticker: EBML container bytes that the
// media store happily kept under an image label.
func WebMSticker() []byte {
	return append([]byte{0x1a, 0x45, 0xdf, 0xa3}, bytes.Repeat([]byte{0x11}, 600)...)
}

// TGSSticker mimics a Telegram animated sticker: gzipped Lottie JSON.
func TGSSticker(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(`{"v":"5.5.2","w":512,"h":512,"ip":0,"op":60,"fr":30,"layers":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// MovingStickerTGS is a red square crossing a 512x512 canvas, so the sampled
// frames are genuinely different from one another.
func MovingStickerTGS(t *testing.T) []byte {
	t.Helper()
	const document = `{"v":"5.5.2","w":512,"h":512,"ip":0,"op":60,"fr":30,"layers":[{"ty":1,"ind":1,"ip":0,"op":60,"st":0,"sw":128,"sh":128,"sc":"#ff0000","ks":{"o":{"a":0,"k":100},"r":{"a":0,"k":0},"p":{"a":1,"k":[{"t":0,"s":[0,128,0],"e":[384,128,0]},{"t":59,"s":[384,128,0]}]},"a":{"a":0,"k":[0,0,0]},"s":{"a":0,"k":[100,100,100]}}}]}`
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(document)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ImageAssets is a loader that serves the named assets as PNG and gives every
// stored file an access path under the media directory.
func ImageAssets(t *testing.T, assets map[string][]byte) *AssetLoader {
	t.Helper()
	return &AssetLoader{
		OpenFn: func(_ context.Context, _, contentHash string) (io.ReadCloser, string, error) {
			data, ok := assets[contentHash]
			if !ok {
				t.Fatalf("unexpected content hash %q", contentHash)
			}
			return io.NopCloser(bytes.NewReader(data)), "image/png", nil
		},
		AccessPathFn: func(_ context.Context, _, contentHash string) (string, error) {
			return "/data/.memoh/media/" + contentHash, nil
		},
	}
}

// DataURL is an image data URL.
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// DecodeDataURLImage decodes the image a data URL carries.
func DecodeDataURLImage(t *testing.T, dataURL string) image.Image {
	t.Helper()
	_, body, found := strings.Cut(dataURL, ",")
	if !found {
		t.Fatalf("not a data URL: %.40q", dataURL)
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
