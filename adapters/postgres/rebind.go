package postgres

import (
	"strconv"
	"strings"
)

// Rebind rewrites SQLite-style placeholders to PostgreSQL ones:
//
//   - '?'    becomes $N where N is one more than the largest index used so far
//     (SQLite's rule), so plain '?' lists become $1, $2, ...
//   - '?NNN' becomes $NNN (SQLite numbered parameters).
//   - '??'   becomes a literal '?' (escape for PostgreSQL operators such as jsonb ?).
//
// Placeholders are not recognised inside single-quoted strings (including
// E'...' strings with backslash escapes), double-quoted identifiers,
// dollar-quoted strings ($$...$$, $tag$...$tag$), -- line comments or /* */
// block comments (which nest in PostgreSQL).
func Rebind(q string) string {
	if strings.IndexByte(q, '?') < 0 {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	maxN := 0
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'':
			escapes := i > 0 && (q[i-1] == 'E' || q[i-1] == 'e') && (i < 2 || !isIdent(q[i-2]))
			j := skipQuoted(q, i, '\'', escapes)
			b.WriteString(q[i:j])
			i = j
		case c == '"':
			j := skipQuoted(q, i, '"', false)
			b.WriteString(q[i:j])
			i = j
		case c == '-' && i+1 < n && q[i+1] == '-':
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = n
			} else {
				j += i + 1
			}
			b.WriteString(q[i:j])
			i = j
		case c == '/' && i+1 < n && q[i+1] == '*':
			j := skipBlockComment(q, i)
			b.WriteString(q[i:j])
			i = j
		case c == '$' && (i == 0 || !isIdent(q[i-1])):
			if j, ok := skipDollarQuoted(q, i); ok {
				b.WriteString(q[i:j])
				i = j
				continue
			}
			b.WriteByte(c)
			i++
		case c == '?':
			if i+1 < n && q[i+1] == '?' {
				b.WriteByte('?')
				i += 2
				continue
			}
			j := i + 1
			for j < n && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			var idx int
			if j > i+1 {
				idx, _ = strconv.Atoi(q[i+1 : j])
			} else {
				idx = maxN + 1
			}
			maxN = max(maxN, idx)
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(idx))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// skipQuoted returns the index after the quoted token starting at q[i]==quote.
// A doubled quote is an escaped quote; with backslash escapes a backslash
// escapes the next byte. An unterminated token extends to the end.
func skipQuoted(q string, i int, quote byte, backslash bool) int {
	n := len(q)
	for j := i + 1; j < n; j++ {
		switch q[j] {
		case '\\':
			if backslash {
				j++
			}
		case quote:
			if j+1 < n && q[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return n
}

func skipBlockComment(q string, i int) int {
	depth := 0
	n := len(q)
	for j := i; j < n; {
		switch {
		case j+1 < n && q[j] == '/' && q[j+1] == '*':
			depth++
			j += 2
		case j+1 < n && q[j] == '*' && q[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return n
}

// skipDollarQuoted handles $tag$...$tag$ starting at q[i]=='$'. It reports false
// when q[i:] does not start a dollar quote (e.g. a $1 parameter).
func skipDollarQuoted(q string, i int) (int, bool) {
	n := len(q)
	j := i + 1
	if j < n && q[j] >= '0' && q[j] <= '9' {
		return 0, false
	}
	for j < n && q[j] != '$' {
		c := q[j]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80) {
			return 0, false
		}
		j++
	}
	if j >= n {
		return 0, false
	}
	tag := q[i : j+1]
	end := strings.Index(q[j+1:], tag)
	if end < 0 {
		return n, true
	}
	return j + 1 + end + len(tag), true
}
