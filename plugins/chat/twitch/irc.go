package main

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
)

type ircMessage struct {
	Tags     map[string]string
	Prefix   string
	Command  string
	Params   []string
	Trailing string
}

func parseIRC(line string) (ircMessage, error) {
	var message ircMessage
	rest := line

	if strings.HasPrefix(rest, "@") {
		end := strings.IndexByte(rest, ' ')
		if end < 0 {
			return message, fmt.Errorf("IRC tags without command")
		}
		message.Tags = parseTags(rest[1:end])
		rest = strings.TrimLeft(rest[end+1:], " ")
	}

	if strings.HasPrefix(rest, ":") {
		end := strings.IndexByte(rest, ' ')
		if end < 0 {
			return message, fmt.Errorf("IRC prefix without command")
		}
		message.Prefix = rest[1:end]
		rest = strings.TrimLeft(rest[end+1:], " ")
	}

	if end := strings.Index(rest, " :"); end >= 0 {
		message.Trailing = rest[end+2:]
		rest = rest[:end]
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return message, fmt.Errorf("empty IRC command")
	}
	message.Command = strings.ToUpper(fields[0])
	if len(fields) > 1 {
		message.Params = fields[1:]
	}
	return message, nil
}

func parseTags(raw string) map[string]string {
	tags := make(map[string]string)
	for _, item := range strings.Split(raw, ";") {
		key, value, found := strings.Cut(item, "=")
		if !found {
			tags[key] = ""
			continue
		}
		tags[key] = unescapeTag(value)
	}
	return tags
}

func unescapeTag(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			out.WriteByte(value[i])
			continue
		}

		i++
		switch value[i] {
		case 's':
			out.WriteByte(' ')
		case ':':
			out.WriteByte(';')
		case '\\':
			out.WriteByte('\\')
		case 'r':
			out.WriteByte('\r')
		case 'n':
			out.WriteByte('\n')
		default:
			out.WriteByte(value[i])
		}
	}
	return out.String()
}

func userLogin(prefix string) string {
	if end := strings.IndexByte(prefix, '!'); end >= 0 {
		return strings.ToLower(prefix[:end])
	}
	return strings.ToLower(prefix)
}

func channelName(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "#"))
}

func validLogin(value string) bool {
	if len(value) < 1 || len(value) > 25 {
		return false
	}
	for _, r := range strings.ToLower(value) {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

type emoteRange struct {
	ID         string
	Start, End int
}

// parseEmoteRanges parses Twitch's "emotes" IRCv3 tag. Offsets are inclusive
// UTF-16 code-unit indexes; malformed ranges are ignored.
func parseEmoteRanges(tag string) []emoteRange {
	var ranges []emoteRange

	for _, group := range strings.Split(tag, "/") {
		id, positions, found := strings.Cut(group, ":")
		if !found || !digitsOnly(id) {
			continue
		}
		for _, position := range strings.Split(positions, ",") {
			startText, endText, found := strings.Cut(position, "-")
			if !found {
				continue
			}
			start, startErr := strconv.Atoi(startText)
			end, endErr := strconv.Atoi(endText)
			if startErr == nil && endErr == nil && start >= 0 && end >= start {
				ranges = append(ranges, emoteRange{ID: id, Start: start, End: end})
			}
		}
	}

	return ranges
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// utf16Offset converts a UTF-16 code-unit offset into a UTF-8 byte offset.
// Offsets splitting a surrogate pair are rejected rather than corrupting text.
func utf16Offset(value string, target int) (int, bool) {
	if target < 0 {
		return 0, false
	}

	units, bytes := 0, 0
	if target == 0 {
		return 0, true
	}

	for _, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units == target {
			return bytes, true
		}
		if units+width > target {
			return 0, false
		}
		units += width
		bytes += len(string(r))
	}

	return bytes, units == target
}

func renderEmotes(text, emotesTag string) string {
	type replacement struct {
		start  int
		end    int
		markup string
	}

	var replacements []replacement
	for _, emote := range parseEmoteRanges(emotesTag) {
		start, startOK := utf16Offset(text, emote.Start)
		end, endOK := utf16Offset(text, emote.End+1)
		if !startOK || !endOK || end <= start {
			continue
		}

		alt := html.EscapeString(text[start:end])
		url := "https://static-cdn.jtvnw.net/emoticons/v2/" + emote.ID + "/default/dark/3.0"
		markup := "<img class=\"chat-emote\" alt=\"" + alt + "\" src=\"" + url + "\" loading=\"lazy\">"
		replacements = append(replacements, replacement{
			start: start,
			end: end,
			markup: markup,
		})
	}

	if len(replacements) == 0 {
		return ""
	}

	sort.Slice(replacements, func(i, j int) bool {
		if replacements[i].start == replacements[j].start {
			return replacements[i].end < replacements[j].end
		}
		return replacements[i].start < replacements[j].start
	})

	var output strings.Builder
	offset := 0
	for _, item := range replacements {
		if item.start < offset || item.start < 0 || item.end > len(text) {
			continue
		}
		output.WriteString(html.EscapeString(text[offset:item.start]))
		output.WriteString(item.markup)
		offset = item.end
	}
	output.WriteString(html.EscapeString(text[offset:]))
	return output.String()
}
