package sessions

import "strings"

// titleMaxRunes bounds a conversation title taken from its first message.
const titleMaxRunes = 60

// titleFrom makes a title from a first message: whitespace runs collapse to
// one space, and the result keeps at most 60 runes.
func titleFrom(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if r := []rune(t); len(r) > titleMaxRunes {
		t = strings.TrimRight(string(r[:titleMaxRunes]), " ")
	}
	return t
}
