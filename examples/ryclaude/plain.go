package main

import (
	"bytes"
	"io"
	"strings"
	"unicode"
)

// plain is what ryclaude says on a terminal, with nothing in it but text.
//
// Much of what it says is somebody else's words: a daemon's name for a key,
// the owner it gives, the message of its refusal. A terminal obeys what it
// is shown — an escape sequence clears the screen, moves the cursor, rewrites
// the line above — so a daemon that could put one in a name could make this
// program appear to have said anything. Everything said goes through here,
// rather than each such string being remembered where it is printed.
type plain struct{ to io.Writer }

func (p plain) Write(said []byte) (int, error) {
	if _, err := p.to.Write(bytes.Map(legible, said)); err != nil {
		return 0, err
	}
	return len(said), nil
}

// legible is a character as a terminal may be given it: itself when it is one
// a person reads, and otherwise the mark of something that was not.
func legible(r rune) rune {
	if text(r) || r == '\n' || r == '\t' {
		return r
	}
	return unicode.ReplacementChar
}

// text says whether a character is one a person reads: a letter, a mark, a
// digit, punctuation, a symbol or a space. Control characters are not, nor
// the format characters that reorder or hide what is beside them.
func text(r rune) bool { return unicode.IsGraphic(r) }

// allText says whether a string is text throughout, and is UTF-8.
func allText(s string) bool {
	return strings.ToValidUTF8(s, "") == s && !strings.ContainsFunc(s, func(r rune) bool { return !text(r) })
}
