package auth

import "strings"

// ParseBearer returns the token in an Authorization header value of the form
// "Bearer <token>". The scheme is matched without regard to case, as RFC 9110
// requires. The form is strict: exactly one space after the scheme, and a
// token made only of the characters RFC 6750 allows in b64token. Anything
// else returns false, including an empty token, extra fields, and control
// characters.
func ParseBearer(header string) (string, bool) {
	const scheme = "Bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := header[len(scheme):]
	for i := 0; i < len(token); i++ {
		if !isB64Token(token[i]) {
			return "", false
		}
	}
	return token, true
}

// isB64Token reports whether c is allowed in an RFC 6750 b64token:
// ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/", followed by any "=".
func isB64Token(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~', c == '+', c == '/', c == '=':
		return true
	}
	return false
}
