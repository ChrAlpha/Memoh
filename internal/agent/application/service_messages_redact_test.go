package application

import (
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
