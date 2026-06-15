package discord

import (
	"testing"

	"github.com/memohai/memoh/internal/channel"
)

func TestParseDiscordMessageParts_PlainTextReturnsNil(t *testing.T) {
	t.Parallel()
	if got := parseDiscordMessageParts("hello world"); got != nil {
		t.Fatalf("expected nil for plain text, got %+v", got)
	}
}

func TestParseDiscordMessageParts_Bold(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("hi **shout** bye")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %+v", parts)
	}
	if parts[0].Text != "hi " || len(parts[0].Styles) != 0 {
		t.Fatalf("part 0 wrong: %+v", parts[0])
	}
	if parts[1].Text != "shout" || len(parts[1].Styles) != 1 || parts[1].Styles[0] != channel.MessageStyleBold {
		t.Fatalf("part 1 wrong: %+v", parts[1])
	}
	if parts[2].Text != " bye" {
		t.Fatalf("part 2 wrong: %+v", parts[2])
	}
}

func TestParseDiscordMessageParts_ItalicStarAndUnderscore(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"*it*", "_it_"} {
		src := src
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			parts := parseDiscordMessageParts(src)
			if len(parts) != 1 || parts[0].Text != "it" {
				t.Fatalf("got %+v", parts)
			}
			if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleItalic {
				t.Fatalf("expected italic style, got %+v", parts[0].Styles)
			}
		})
	}
}

func TestParseDiscordMessageParts_Strikethrough(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("~~gone~~")
	if len(parts) != 1 || parts[0].Text != "gone" {
		t.Fatalf("got %+v", parts)
	}
	if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleStrikethrough {
		t.Fatalf("expected strikethrough, got %+v", parts[0].Styles)
	}
}

func TestParseDiscordMessageParts_InlineCode(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("see `foo()` here")
	if len(parts) != 3 {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].Text != "foo()" || len(parts[1].Styles) != 1 || parts[1].Styles[0] != channel.MessageStyleCode {
		t.Fatalf("inline code wrong: %+v", parts[1])
	}
}

func TestParseDiscordMessageParts_CodeBlockWithLanguage(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("```rust\nfn main(){}\n```")
	if len(parts) != 1 || parts[0].Type != channel.MessagePartCodeBlock {
		t.Fatalf("got %+v", parts)
	}
	if parts[0].Language != "rust" || parts[0].Text != "fn main(){}" {
		t.Fatalf("expected pre rust 'fn main(){}', got %+v", parts[0])
	}
}

func TestParseDiscordMessageParts_CodeBlockNoLanguage(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("```\nx\n```")
	if len(parts) != 1 || parts[0].Type != channel.MessagePartCodeBlock || parts[0].Language != "" {
		t.Fatalf("got %+v", parts)
	}
	if parts[0].Text != "x" {
		t.Fatalf("expected leaf text 'x', got %q", parts[0].Text)
	}
}

func TestParseDiscordMessageParts_Link(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("see [Memoh](https://example.com)")
	if len(parts) != 2 {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].Type != channel.MessagePartLink || parts[1].URL != "https://example.com" || parts[1].Text != "Memoh" {
		t.Fatalf("link wrong: %+v", parts[1])
	}
}

func TestParseDiscordMessageParts_LinkWithBalancedParensInURL(t *testing.T) {
	t.Parallel()
	// Wikipedia-style URLs frequently embed unescaped parens. Greedy "first ')'"
	// matching truncates the href and dumps the rest as plain text, so the
	// LLM sees a broken link target. Track depth instead.
	parts := parseDiscordMessageParts("see [Foo](https://en.wikipedia.org/wiki/Foo_(bar))")
	var link *channel.MessagePart
	for i := range parts {
		if parts[i].Type == channel.MessagePartLink {
			link = &parts[i]
			break
		}
	}
	if link == nil {
		t.Fatalf("expected a link part, got %+v", parts)
	}
	if link.URL != "https://en.wikipedia.org/wiki/Foo_(bar)" {
		t.Fatalf("expected full URL with parens preserved, got %q", link.URL)
	}
	if link.Text != "Foo" {
		t.Fatalf("expected label 'Foo', got %q", link.Text)
	}
}

func TestParseDiscordMessageParts_LinkWithUnbalancedParensFallsThrough(t *testing.T) {
	t.Parallel()
	// "[A](https://x.com/foo" never closes the URL paren — emit the input as
	// plain rather than swallowing it as a corrupted link.
	if got := parseDiscordMessageParts("[A](https://x.com/foo"); got != nil {
		t.Fatalf("expected nil for unbalanced link, got %+v", got)
	}
}

func TestParseDiscordMessageParts_UserMentionAndNickname(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"<@123>", "<@!123>"} {
		src := src
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			parts := parseDiscordMessageParts("hi " + src)
			if len(parts) != 2 || parts[1].Type != channel.MessagePartMention {
				t.Fatalf("got %+v", parts)
			}
			if parts[1].ChannelIdentityID != "123" {
				t.Fatalf("expected channel_identity_id=123, got %q", parts[1].ChannelIdentityID)
			}
		})
	}
}

func TestParseDiscordMessageParts_ChannelMention(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("see <#456>")
	if len(parts) != 2 || parts[1].Type != channel.MessagePartMention {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].ChannelIdentityID != "456" {
		t.Fatalf("expected channel id 456, got %q", parts[1].ChannelIdentityID)
	}
}

func TestParseDiscordMessageParts_CustomEmojiBecomesText(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("nice <:dance:123>")
	if len(parts) != 2 {
		t.Fatalf("got %+v", parts)
	}
	if parts[1].Type != channel.MessagePartText || parts[1].Text != ":dance:" {
		t.Fatalf("expected :dance: text, got %+v", parts[1])
	}
}

func TestParseDiscordMessageParts_DoubleUnderscoreStaysPlain(t *testing.T) {
	t.Parallel()
	// Discord's __underline__ has no canonical-schema mapping. The previous
	// parser skipped the first underscore but then matched the second one as
	// an italic opener and the closing pair's first underscore as its close,
	// producing a corrupted "_<i>important</i>_" span. Treat the entire
	// __..__ pair as literal so the LLM at least sees the original text.
	if got := parseDiscordMessageParts("__important__"); got != nil {
		t.Fatalf("expected nil (whole span treated as plain), got %+v", got)
	}
}

func TestParseDiscordMessageParts_DoubleUnderscoreCoexistsWithItalic(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("*it* __foo__ bye")
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (italic + trailing plain), got %+v", parts)
	}
	if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleItalic || parts[0].Text != "it" {
		t.Fatalf("part 0 wrong: %+v", parts[0])
	}
	if parts[1].Type != channel.MessagePartText || parts[1].Text != " __foo__ bye" {
		t.Fatalf("expected plain ' __foo__ bye', got %+v", parts[1])
	}
}

func TestParseDiscordMessageParts_BoldOuterWinsOverItalicAmbiguity(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts("**bold _italic_ end**")
	if len(parts) != 1 {
		t.Fatalf("expected single bold part covering whole span, got %+v", parts)
	}
	if len(parts[0].Styles) != 1 || parts[0].Styles[0] != channel.MessageStyleBold {
		t.Fatalf("expected bold style, got %+v", parts[0].Styles)
	}
	if parts[0].Text != "bold _italic_ end" {
		t.Fatalf("expected inner text preserved as plain (no recursion in v1), got %q", parts[0].Text)
	}
}

func TestParseDiscordMessageParts_BackslashesNotInterpreted(t *testing.T) {
	t.Parallel()
	parts := parseDiscordMessageParts(`a\*b\*c`)
	if parts != nil {
		t.Fatalf("expected nil — escapes are passed through, no rich content, got %+v", parts)
	}
}

func TestParseDiscordMessageParts_UnterminatedBoldFallsThrough(t *testing.T) {
	t.Parallel()
	if got := parseDiscordMessageParts("**unterminated"); got != nil {
		t.Fatalf("expected nil for unterminated bold (no rich content), got %+v", got)
	}
}

func TestParseDiscordMessageParts_EmptyInputReturnsNil(t *testing.T) {
	t.Parallel()
	if got := parseDiscordMessageParts(""); got != nil {
		t.Fatalf("got %+v", got)
	}
}
