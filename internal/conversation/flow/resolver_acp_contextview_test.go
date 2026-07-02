package flow

import (
	"context"
	"testing"
)

func TestACPContextViaContextViewPassThrough(t *testing.T) {
	t.Parallel()

	markdown := "# Memoh ACP Context\n\ncontent body"
	gotMarkdown, gotURI := acpContextViaContextView(context.Background(), nil, markdown)

	if gotMarkdown != markdown {
		t.Fatalf("markdown = %q, want byte-for-byte pass-through", gotMarkdown)
	}
	if gotURI != acpContextURI {
		t.Fatalf("uri = %q, want %q", gotURI, acpContextURI)
	}
}
