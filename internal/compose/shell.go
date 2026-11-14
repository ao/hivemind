package compose

import (
	"errors"
	"strings"
)

// splitCommand splits a command string into words by POSIX shell quoting
// rules, and expands nothing:
//
//   - spaces, tabs and newlines outside quotes separate words;
//   - 'single quotes' keep everything up to the next single quote;
//   - in "double quotes", a backslash escapes $, `, ", \ and a newline, and
//     is kept before any other character;
//   - outside quotes, a backslash keeps the next character, and a backslash
//     before a newline joins the lines.
//
// $, *, ;, | and the other shell operators are ordinary characters.
func splitCommand(s string) ([]string, error) {
	var (
		words  []string
		word   strings.Builder
		inWord bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("a single quote is not closed")
			}
			word.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			closed := false
			for i++; i < len(s); i++ {
				if s[i] == '"' {
					closed = true
					break
				}
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
					i++
					if s[i] == '\n' {
						continue
					}
				}
				word.WriteByte(s[i])
			}
			if !closed {
				return nil, errors.New("a double quote is not closed")
			}
			inWord = true
		case c == '\\':
			if i+1 == len(s) {
				return nil, errors.New("the command ends with a backslash")
			}
			i++
			if s[i] == '\n' {
				continue
			}
			word.WriteByte(s[i])
			inWord = true
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}
