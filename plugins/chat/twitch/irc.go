package main

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
)

type ircMessage struct {
	Tags map[string]string
	Prefix, Command string
	Params []string
	Trailing string
}

func parseIRC(line string) (ircMessage, error) {
	var m ircMessage
	s := line
	if strings.HasPrefix(s, "@") {
		i := strings.IndexByte(s, ' ')
		if i < 0 { return m, fmt.Errorf("IRC tags without command") }
		m.Tags = parseTags(s[1:i])
		s = strings.TrimLeft(s[i+1:], " ")
	}
	if strings.HasPrefix(s, ":") {
		i := strings.IndexByte(s, ' ')
		if i < 0 { return m, fmt.Errorf("IRC prefix without command") }
		m.Prefix = s[1:i]
		s = strings.TrimLeft(s[i+1:], " ")
	}
	if i := strings.Index(s, " :"); i >= 0 {
		m.Trailing = s[i+2:]
		s = s[:i]
	}
	fields := strings.Fields(s)
	if len(fields) == 0 { return m, fmt.Errorf("empty IRC command") }
	m.Command = strings.ToUpper(fields[0])
	if len(fields) > 1 { m.Params = fields[1:] }
	return m, nil
}

func parseTags(raw string) map[string]string {
	out := make(map[string]string)
	for _, item := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(item, "=")
		if !ok { out[k] = ""; continue }
		out[k] = unescapeTag(v)
	}
	return out
}

func unescapeTag(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) { b.WriteByte(s[i]); continue }
		i++
		switch s[i] {
		case 's': b.WriteByte(' ')
		case ':': b.WriteByte(';')
		case '\\': b.WriteByte('\\')
		case 'r': b.WriteByte('\r')
		case 'n': b.WriteByte('\n')
		default: b.WriteByte(s[i])
		}
	}
	return b.String()
}

func userLogin(prefix string) string {
	if i := strings.IndexByte(prefix, '!'); i >= 0 { return strings.ToLower(prefix[:i]) }
	return strings.ToLower(prefix)
}
func channelName(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "#") }
func validLogin(s string) bool {
	if len(s) < 1 || len(s) > 25 { return false }
	for _, r := range strings.ToLower(s) {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') { return false }
	}
	return true
}

type emoteRange struct { ID string; Start, End int }
func parseEmoteRanges(tag string) []emoteRange {
	var out []emoteRange
	for _, group := range strings.Split(tag, "/") {
		id, positions, ok := strings.Cut(group, ":")
		if !ok || id == "" { continue }
		for _, pos := range strings.Split(positions, ",") {
			a, b, ok := strings.Cut(pos, "-")
			if !ok { continue }
			start, e1 := strconv.Atoi(a); end, e2 := strconv.Atoi(b)
			if e1 == nil && e2 == nil && start >= 0 && end >= start {
				out = append(out, emoteRange{ID:id, Start:start, End:end})
			}
		}
	}
	return out
}

// Twitch emote offsets are inclusive UTF-16 code-unit positions.
func utf16Offset(s string, target int) (int, bool) {
	units, bytes := 0, 0
	if target == 0 { return 0, true }
	for _, r := range s {
		n := 1
		if r > 0xffff { n = 2 }
		if units == target { return bytes, true }
		if units+n > target { return 0, false }
		units += n
		bytes += len(string(r))
	}
	return bytes, units == target
}

func renderEmotes(text, emotesTag, gifTag string) string {
	ranges := append(parseEmoteRanges(emotesTag), parseEmoteRanges(gifTag)...)
	type replacement struct { start, end int; html string }
	var rs []replacement
	for _, r := range ranges {
		start, ok1 := utf16Offset(text, r.Start)
		end, ok2 := utf16Offset(text, r.End+1)
		if !ok1 || !ok2 || end <= start { continue }
		alt := html.EscapeString(text[start:end])
		url := "https://static-cdn.jtvnw.net/emoticons/v2/" + r.ID + "/default/dark/3.0"
		rs = append(rs, replacement{start, end, "<img class=\"chat-emote\" alt=\"" + alt + "\" src=\"" + url + "\" loading=\"lazy\">"})
	}
	if len(rs) == 0 { return "" }
	sort.Slice(rs, func(i,j int) bool { return rs[i].start < rs[j].start })
	var b strings.Builder
	at := 0
	for _, r := range rs {
		if r.start < at || r.start < 0 || r.end > len(text) { continue }
		b.WriteString(html.EscapeString(text[at:r.start]))
		b.WriteString(r.html)
		at = r.end
	}
	b.WriteString(html.EscapeString(text[at:]))
	return b.String()
}
