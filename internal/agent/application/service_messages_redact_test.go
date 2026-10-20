package application

import (
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactMediaPartsForStorage_ReplacesFilePartWithPlaceholder(t *testing.T) {
	msgs := []sdk.Message{{
		Role: sdk.MessageRoleUser,
		Content: []sdk.MessagePart{
			sdk.FilePart{Data: strings.Repeat("A", 1024), MediaType: "application/pdf", Filename: "report.pdf"},
		},
	}}

	out := redactMediaPartsForStorage(msgs)
	require.Len(t, out, 1)
	require.Len(t, out[0].Content, 1)
	tp, ok := out[0].Content[0].(sdk.TextPart)
	require.True(t, ok, "expected TextPart, got %T", out[0].Content[0])
	assert.Contains(t, tp.Text, "report.pdf")
	assert.Contains(t, tp.Text, "application/pdf")
	assert.NotContains(t, tp.Text, "AAAA")

	// Input slice must not be mutated: the live turn still needs the bytes.
	_, stillFile := msgs[0].Content[0].(sdk.FilePart)
	assert.True(t, stillFile)
}

func TestRedactMediaPartsForStorage_KeepsSiblingParts(t *testing.T) {
	msgs := []sdk.Message{{
		Role: sdk.MessageRoleUser,
		Content: []sdk.MessagePart{
			sdk.TextPart{Text: "please read"},
			sdk.FilePart{Data: "JVBERi0=", MediaType: "application/pdf"},
			sdk.ImagePart{Image: "data:image/png;base64,abc", MediaType: "image/png"},
		},
	}}

	out := redactMediaPartsForStorage(msgs)
	require.Len(t, out[0].Content, 3)
	assert.Equal(t, sdk.TextPart{Text: "please read"}, out[0].Content[0])
	assert.IsType(t, sdk.TextPart{}, out[0].Content[1])
	image, ok := out[0].Content[2].(sdk.TextPart)
	require.True(t, ok, "expected TextPart, got %T", out[0].Content[2])
	assert.Contains(t, image.Text, "image/png")
	assert.NotContains(t, image.Text, "abc")
}

func TestRedactMediaPartsForStorage_NoMediaPartsPassthrough(t *testing.T) {
	msgs := []sdk.Message{{
		Role:    sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.TextPart{Text: "hello"}},
	}}
	out := redactMediaPartsForStorage(msgs)
	assert.Equal(t, msgs, out)
}

func TestSDKMessagesToModelMessages_NeverPersistsFilePartBytes(t *testing.T) {
	payload := strings.Repeat("JVBERi0xLjQ", 100)
	msgs := []sdk.Message{{
		Role: sdk.MessageRoleUser,
		Content: []sdk.MessagePart{
			sdk.FilePart{Data: payload, MediaType: "application/pdf", Filename: "big.pdf"},
		},
	}}
	converted := sdkMessagesToModelMessages(msgs)
	require.Len(t, converted, 1)
	assert.NotContains(t, string(converted[0].Content), payload[:64])
	assert.Contains(t, string(converted[0].Content), "big.pdf")
}
