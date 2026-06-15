package slack

import (
	"strings"
	"unicode/utf8"

	"github.com/memohai/memoh/internal/channel"
)

func parseSlackMessageParts(text string) []channel.MessagePart {
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
			if part, n, ok := trySlackCodeBlock(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := trySlackInlineCode(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := trySlackAngleToken(text[i:]); ok {
				flush()
				parts = append(parts, part)
				i += n
				rich = true
				continue
			}
			if part, n, ok := trySlackStyled(text[i:]); ok {
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

func trySlackCodeBlock(s string) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, "```") {
		return channel.MessagePart{}, 0, false
	}
	rest := s[3:]
	end := strings.Index(rest, "```")
	if end < 0 {
		return channel.MessagePart{}, 0, false
	}
	body := strings.TrimPrefix(rest[:end], "\n")
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		return channel.MessagePart{}, 0, false
	}
	return channel.MessagePart{
		Type: channel.MessagePartCodeBlock,
		Text: body,
	}, 3 + end + 3, true
}

func trySlackInlineCode(s string) (channel.MessagePart, int, bool) {
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

func trySlackAngleToken(s string) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, "<") {
		return channel.MessagePart{}, 0, false
	}
	end := strings.IndexByte(s, '>')
	if end < 2 {
		return channel.MessagePart{}, 0, false
	}
	inner := s[1:end]
	n := end + 1

	label := ""
	if pipe := strings.IndexByte(inner, '|'); pipe >= 0 {
		label = inner[pipe+1:]
		inner = inner[:pipe]
	}

	switch {
	case strings.HasPrefix(inner, "@"):
		id := strings.TrimSpace(inner[1:])
		if id == "" {
			return channel.MessagePart{}, 0, false
		}
		display := strings.TrimSpace(label)
		if display == "" {
			display = "@" + id
		} else if !strings.HasPrefix(display, "@") {
			display = "@" + display
		}
		return channel.MessagePart{
			Type:              channel.MessagePartMention,
			Text:              display,
			ChannelIdentityID: id,
		}, n, true
	case strings.HasPrefix(inner, "#"):
		id := strings.TrimSpace(inner[1:])
		if id == "" {
			return channel.MessagePart{}, 0, false
		}
		display := strings.TrimSpace(label)
		if display == "" {
			display = "#" + id
		} else if !strings.HasPrefix(display, "#") {
			display = "#" + display
		}
		return channel.MessagePart{
			Type:              channel.MessagePartMention,
			Text:              display,
			ChannelIdentityID: id,
		}, n, true
	case strings.HasPrefix(inner, "!"):
		token := strings.TrimSpace(inner[1:])
		if token == "" {
			return channel.MessagePart{}, 0, false
		}
		display := strings.TrimSpace(label)
		if display == "" {
			display = "@" + token
		} else if !strings.HasPrefix(display, "@") {
			display = "@" + display
		}
		return channel.MessagePart{
			Type: channel.MessagePartMention,
			Text: display,
		}, n, true
	default:
		url := strings.TrimSpace(inner)
		if url == "" {
			return channel.MessagePart{}, 0, false
		}
		display := strings.TrimSpace(label)
		if display == "" {
			display = url
		}
		return channel.MessagePart{
			Type: channel.MessagePartLink,
			Text: display,
			URL:  url,
		}, n, true
	}
}

func trySlackStyled(s string) (channel.MessagePart, int, bool) {
	if part, n, ok := trySlackDelimited(s, "*", channel.MessageStyleBold); ok {
		return part, n, ok
	}
	if part, n, ok := trySlackDelimited(s, "_", channel.MessageStyleItalic); ok {
		return part, n, ok
	}
	if part, n, ok := trySlackDelimited(s, "~", channel.MessageStyleStrikethrough); ok {
		return part, n, ok
	}
	return channel.MessagePart{}, 0, false
}

func trySlackDelimited(s, delim string, style channel.MessageTextStyle) (channel.MessagePart, int, bool) {
	if !strings.HasPrefix(s, delim) {
		return channel.MessagePart{}, 0, false
	}
	rest := s[len(delim):]
	if rest == "" {
		return channel.MessagePart{}, 0, false
	}
	if rest[0] == delim[0] {
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
