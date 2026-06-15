package slack

import (
	"testing"

	"github.com/memohai/memoh/internal/channel"
)

func TestParseSlackMessageParts_PlainTextReturnsNil(t *testing.T) {
	t.Parallel()
	if got := parseSlackMessageParts("hello world"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestParseSlackMessageParts_BoldSingleAsterisk(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("hi *shout* bye")
	if len(parts) != 3 {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].Text != "shout" || len(parts[1].Styles) != 1 || parts[1].Styles[0] != channel.MessageStyleBold {
		t.Fatalf("part 1 wrong: %+v", parts[1])
	}
}

func TestParseSlackMessageParts_ItalicUnderscore(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("_lean_")
	if len(parts) != 1 || parts[0].Text != "lean" {
		t.Fatalf("got %+v", parts)
	}
	if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleItalic {
		t.Fatalf("styles wrong: %+v", parts[0].Styles)
	}
}

func TestParseSlackMessageParts_StrikethroughSingleTilde(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("~gone~")
	if len(parts) != 1 || parts[0].Text != "gone" {
		t.Fatalf("got %+v", parts)
	}
	if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleStrikethrough {
		t.Fatalf("styles wrong: %+v", parts[0].Styles)
	}
}

func TestParseSlackMessageParts_InlineCode(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("call `foo()` now")
	if len(parts) != 3 {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].Text != "foo()" || len(parts[1].Styles) != 1 || parts[1].Styles[0] != channel.MessageStyleCode {
		t.Fatalf("inline code wrong: %+v", parts[1])
	}
}

func TestParseSlackMessageParts_CodeBlock(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("```\nfn main(){}\n```")
	if len(parts) != 1 || parts[0].Type != channel.MessagePartCodeBlock {
		t.Fatalf("got %+v", parts)
	}
	if parts[0].Text != "fn main(){}" {
		t.Fatalf("expected leaf text, got %q", parts[0].Text)
	}
	if parts[0].Language != "" {
		t.Fatalf("Slack mrkdwn code blocks have no language hint, got %q", parts[0].Language)
	}
}

func TestParseSlackMessageParts_UserMention(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("hi <@U123>")
	if len(parts) != 2 || parts[1].Type != channel.MessagePartMention {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].ChannelIdentityID != "U123" {
		t.Fatalf("expected channel_identity_id=U123, got %q", parts[1].ChannelIdentityID)
	}
}

func TestParseSlackMessageParts_UserMentionWithLabel(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("hi <@U123|alice>")
	if len(parts) != 2 || parts[1].Type != channel.MessagePartMention {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].ChannelIdentityID != "U123" {
		t.Fatalf("expected channel_identity_id=U123, got %q", parts[1].ChannelIdentityID)
	}
	if parts[1].Text != "@alice" {
		t.Fatalf("expected human label '@alice', got %q", parts[1].Text)
	}
}

func TestParseSlackMessageParts_ChannelMention(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("come to <#C456|general>")
	if len(parts) != 2 || parts[1].Type != channel.MessagePartMention {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].ChannelIdentityID != "C456" {
		t.Fatalf("expected C456, got %q", parts[1].ChannelIdentityID)
	}
	if parts[1].Text != "#general" {
		t.Fatalf("expected '#general', got %q", parts[1].Text)
	}
}

func TestParseSlackMessageParts_LinkWithPipe(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("see <https://example.com|Memoh>")
	if len(parts) != 2 || parts[1].Type != channel.MessagePartLink {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].URL != "https://example.com" || parts[1].Text != "Memoh" {
		t.Fatalf("link wrong: %+v", parts[1])
	}
}

func TestParseSlackMessageParts_BareURL(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("see <https://example.com>")
	if len(parts) != 2 || parts[1].Type != channel.MessagePartLink {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].URL != "https://example.com" || parts[1].Text != "https://example.com" {
		t.Fatalf("bare URL wrong: %+v", parts[1])
	}
}

func TestParseSlackMessageParts_SpecialMentionHere(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("<!here>")
	if len(parts) != 1 || parts[0].Type != channel.MessagePartMention {
		t.Fatalf("got %+v", parts)
	}
	if parts[0].Text != "@here" {
		t.Fatalf("expected @here, got %q", parts[0].Text)
	}
}

func TestParseSlackMessageParts_SubteamMentionWithLabel(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("<!subteam^S0123|@design>")
	if len(parts) != 1 || parts[0].Type != channel.MessagePartMention {
		t.Fatalf("got %+v", parts)
	}
	if parts[0].Text != "@design" {
		t.Fatalf("expected '@design', got %q", parts[0].Text)
	}
}

func TestParseSlackMessageParts_DateTokenBecomesFallbackText(t *testing.T) {
	t.Parallel()
	// <!date^...|fallback> is Slack's date formatter, NOT a broadcast mention.
	// The fallback after the pipe is the human-readable rendered date and is
	// what the LLM should see.
	parts := parseSlackMessageParts("meet at <!date^1518476400^{date_short}|Feb 18>")
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (lead + date text), got %+v", parts)
	}
	if parts[1].Type != channel.MessagePartText {
		t.Fatalf("expected the date token to render as text, got %+v", parts[1])
	}
	if parts[1].Text != "Feb 18" {
		t.Fatalf("expected 'Feb 18', got %q", parts[1].Text)
	}
}

func TestParseSlackMessageParts_UnknownBangTokenFallsToText(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("<!something|whatever>")
	if len(parts) != 1 {
		t.Fatalf("got %+v", parts)
	}
	if parts[0].Type != channel.MessagePartText || parts[0].Text != "whatever" {
		t.Fatalf("expected plain 'whatever', got %+v", parts[0])
	}
}

func TestParseSlackMessageParts_DoubledBoldStaysPlain(t *testing.T) {
	t.Parallel()
	// Pasted GitHub-style "**bold**" uses doubled stars; Slack itself only
	// recognises single "*" as bold. Without intervention the parser would
	// reject the first star then re-enter at the second and emit a bold
	// span containing "bold" surrounded by stray "*" markers, corrupting
	// the user's original text.
	if got := parseSlackMessageParts("**bold**"); got != nil {
		t.Fatalf("expected nil (whole span plain), got %+v", got)
	}
}

func TestParseSlackMessageParts_DoubledStrikeStaysPlain(t *testing.T) {
	t.Parallel()
	if got := parseSlackMessageParts("~~old~~"); got != nil {
		t.Fatalf("expected nil (whole span plain), got %+v", got)
	}
}

func TestParseSlackMessageParts_DoubledMarkersCoexistWithStyles(t *testing.T) {
	t.Parallel()
	parts := parseSlackMessageParts("*hi* **pasted**")
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (bold + trailing plain), got %+v", parts)
	}
	if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleBold || parts[0].Text != "hi" {
		t.Fatalf("part 0 wrong: %+v", parts[0])
	}
	if parts[1].Type != channel.MessagePartText || parts[1].Text != " **pasted**" {
		t.Fatalf("expected trailing ' **pasted**' as plain, got %+v", parts[1])
	}
}

func TestParseSlackMessageParts_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	if got := parseSlackMessageParts(""); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestParseSlackMessageParts_UnterminatedBoldFallsThrough(t *testing.T) {
	t.Parallel()
	if got := parseSlackMessageParts("*unterminated"); got != nil {
		t.Fatalf("expected nil for unterminated bold, got %+v", got)
	}
}

func TestParseSlackMessageParts_SkipsEmptyBody(t *testing.T) {
	t.Parallel()
	if got := parseSlackMessageParts("**"); got != nil {
		t.Fatalf("expected nil for empty bold body, got %+v", got)
	}
}
