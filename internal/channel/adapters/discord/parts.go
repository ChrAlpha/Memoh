package discord

import (
	"strings"
	"unicode/utf8"

	"github.com/memohai/memoh/internal/channel"
)

func parseDiscordMessageParts(text string) []channel.MessagePart {
	if text == "" {
		return nil
	}
	var parts []channel.MessagePart
	var plain strings.Builder
	flush := func() {
		if plain.Len() == 0 {
			return
		}
		parts = append(parts, channel.MessagePart{
			Type: channel.MessagePartText,
			Text: plain.String(),
		})
		plain.Reset()
	}

	i := 0
	escaped := false
	rich := false
	for i < len(text) {
		if !escaped {
			// Discord's __underline__ has no canonical-schema mapping; consume the
			// "__" pair as plain so the trailing single underscore can't slip into
			// the italic parser as a stray opener (which would corrupt the span
			// into "_<i>foo</i>_").
			if i+1 < len(text) && text[i] == '_' && text[i+1] == '_' {
				plain.WriteString("__")
				i += 2
				continue
			}
			if part, n, ok := tryDiscordCodeBlock(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := tryDiscordInlineCode(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := tryDiscordLink(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := tryDiscordMention(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := tryDiscordStyled(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		plain.WriteRune(r)
		escaped = (r == '\\')
		i += size
	}
	flush()

	if !rich {
		return nil
	}
	return parts
}

func tryDiscordCodeBlock(s string) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, "```") {
		return channel.MessagePart{}, 0, false
	}
	rest := s[3:]
	end := strings.Index(rest, "```")
	if end < 0 {
		return channel.MessagePart{}, 0, false
	}
	inner := rest[:end]
	n := 3 + end + 3

	lang := ""
	body := inner
	if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
		candidate := strings.TrimSpace(inner[:nl])
		if candidate != "" && !strings.ContainsAny(candidate, " \t") {
			lang = candidate
		}
		body = inner[nl+1:]
	}
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		return channel.MessagePart{}, 0, false
	}
	return channel.MessagePart{
		Type:     channel.MessagePartCodeBlock,
		Text:     body,
		Language: lang,
	}, n, true
}

func tryDiscordInlineCode(s string) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, "`") || strings.HasPrefix(s, "```") {
		return channel.MessagePart{}, 0, false
	}
	rest := s[1:]
	end := strings.IndexByte(rest, '`')
	if end <= 0 {
		return channel.MessagePart{}, 0, false
	}
	return channel.MessagePart{
		Type:   channel.MessagePartText,
		Text:   rest[:end],
		Styles: []channel.MessageTextStyle{channel.MessageStyleCode},
	}, 1 + end + 1, true
}

func tryDiscordLink(s string) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, "[") {
		return channel.MessagePart{}, 0, false
	}
	closeBracket := strings.IndexByte(s, ']')
	if closeBracket < 1 || closeBracket+1 >= len(s) || s[closeBracket+1] != '(' {
		return channel.MessagePart{}, 0, false
	}
	closeParen := strings.IndexByte(s[closeBracket+2:], ')')
	if closeParen < 0 {
		return channel.MessagePart{}, 0, false
	}
	label := s[1:closeBracket]
	url := s[closeBracket+2 : closeBracket+2+closeParen]
	if label == "" || url == "" {
		return channel.MessagePart{}, 0, false
	}
	return channel.MessagePart{
		Type: channel.MessagePartLink,
		Text: label,
		URL:  url,
	}, closeBracket + 2 + closeParen + 1, true
}

func tryDiscordMention(s string) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, "<") {
		return channel.MessagePart{}, 0, false
	}
	end := strings.IndexByte(s, '>')
	if end < 2 {
		return channel.MessagePart{}, 0, false
	}
	inner := s[1:end]
	n := end + 1
	raw := s[:n]

	if strings.HasPrefix(inner, "@") {
		id := strings.TrimPrefix(inner, "@")
		id = strings.TrimPrefix(id, "!")
		if id == "" || !isAllDigits(id) {
			return channel.MessagePart{}, 0, false
		}
		return channel.MessagePart{
			Type:              channel.MessagePartMention,
			Text:              raw,
			ChannelIdentityID: id,
		}, n, true
	}
	if strings.HasPrefix(inner, "#") {
		id := strings.TrimPrefix(inner, "#")
		if id == "" || !isAllDigits(id) {
			return channel.MessagePart{}, 0, false
		}
		return channel.MessagePart{
			Type:              channel.MessagePartMention,
			Text:              raw,
			ChannelIdentityID: id,
		}, n, true
	}
	body := strings.TrimPrefix(inner, "a")
	if !strings.HasPrefix(body, ":") {
		return channel.MessagePart{}, 0, false
	}
	body = body[1:]
	sep := strings.IndexByte(body, ':')
	if sep <= 0 {
		return channel.MessagePart{}, 0, false
	}
	name := body[:sep]
	if name == "" {
		return channel.MessagePart{}, 0, false
	}
	return channel.MessagePart{
		Type: channel.MessagePartText,
		Text: ":" + name + ":",
	}, n, true
}

func tryDiscordStyled(s string) (channel.MessagePart, int, bool) {
	if part, n, ok := tryDiscordDelimited(s, "**", channel.MessageStyleBold); ok {
		return part, n, ok
	}
	if part, n, ok := tryDiscordDelimited(s, "~~", channel.MessageStyleStrikethrough); ok {
		return part, n, ok
	}
	if part, n, ok := tryDiscordDelimited(s, "*", channel.MessageStyleItalic); ok {
		return part, n, ok
	}
	if part, n, ok := tryDiscordDelimited(s, "_", channel.MessageStyleItalic); ok {
		return part, n, ok
	}
	return channel.MessagePart{}, 0, false
}

func tryDiscordDelimited(s, delim string, style channel.MessageTextStyle) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, delim) {
		return channel.MessagePart{}, 0, false
	}
	rest := s[len(delim):]
	if rest == "" {
		return channel.MessagePart{}, 0, false
	}
	if len(delim) == 1 && rest[0] == delim[0] {
		return channel.MessagePart{}, 0, false
	}
	end := strings.Index(rest, delim)
	if end <= 0 {
		return channel.MessagePart{}, 0, false
	}
	body := rest[:end]
	if strings.TrimSpace(body) == "" {
		return channel.MessagePart{}, 0, false
	}
	return channel.MessagePart{
		Type:   channel.MessagePartText,
		Text:   body,
		Styles: []channel.MessageTextStyle{style},
	}, len(delim) + end + len(delim), true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
