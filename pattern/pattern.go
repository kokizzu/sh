// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

// Package pattern allows working with shell pattern matching notation, also
// known as wildcards or globbing.
//
// For reference, see
// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html#tag_18_13.
package pattern

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Mode can be used to supply a number of options to the package's functions.
// Not all functions change their behavior with all of the options below.
type Mode uint

type SyntaxError struct {
	msg string
	err error
}

func (e SyntaxError) Error() string { return e.msg }

func (e SyntaxError) Unwrap() error { return e.err }

const (
	Shortest     Mode = 1 << iota // prefer the shortest match.
	Filenames                     // "*" and "?" don't match slashes; only "**" does
	Braces                        // support "{a,b}" and "{1..4}"
	EntireString                  // match the entire string using ^$ delimiters
	NoGlobCase                    // Do case-insensitive match (that is, use (?i) in the regexp)
	NoGlobStar                    // Do not support "**"
)

var numRange = regexp.MustCompile(`^([+-]?\d+)\.\.([+-]?\d+)}`)

// Regexp turns a shell pattern into a regular expression that can be used with
// [regexp.Compile]. It will return an error if the input pattern was incorrect.
// Otherwise, the returned expression can be passed to [regexp.MustCompile].
//
// For example, Regexp(`foo*bar?`, true) returns `foo.*bar.`.
//
// Note that this function (and [QuoteMeta]) should not be directly used with file
// paths if Windows is supported, as the path separator on that platform is the
// same character as the escaping character for shell patterns.
func Regexp(pat string, mode Mode) (string, error) {
	needsEscaping := false
noopLoop:
	for _, r := range pat {
		switch r {
		// including those that need escaping since they are
		// regular expression metacharacters
		case '*', '?', '[', '\\', '.', '+', '(', ')', '|',
			']', '{', '}', '^', '$':
			needsEscaping = true
			break noopLoop
		}
	}
	if !needsEscaping && mode&EntireString == 0 { // short-cut without a string copy
		return pat, nil
	}
	closingBraces := []int{}
	var sb strings.Builder
	// Enable matching `\n` with the `.` metacharacter as globs match `\n`
	sb.WriteString("(?s)")
	dotMeta := false
	if mode&NoGlobCase != 0 {
		sb.WriteString("(?i)")
	}
	if mode&EntireString != 0 {
		sb.WriteString("^")
	}
writeLoop:
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			if mode&Filenames != 0 {
				// "**" only acts as globstar if it is alone as a path element.
				singleBefore := i == 0 || pat[i-1] == '/'
				if i++; i < len(pat) && pat[i] == '*' {
					singleAfter := i == len(pat)-1 || pat[i+1] == '/'
					if mode&NoGlobStar != 0 || !singleBefore || !singleAfter {
						sb.WriteString("[^/]*")
					} else if i++; i < len(pat) && pat[i] == '/' {
						sb.WriteString("(.*/|)")
						dotMeta = true
					} else {
						sb.WriteString(".*")
						dotMeta = true
						i--
					}
				} else {
					sb.WriteString("[^/]*")
					i--
				}
			} else {
				sb.WriteString(".*")
				dotMeta = true
			}
			if mode&Shortest != 0 {
				sb.WriteByte('?')
			}
		case '?':
			if mode&Filenames != 0 {
				sb.WriteString("[^/]")
			} else {
				sb.WriteByte('.')
				dotMeta = true
			}
		case '\\':
			if i++; i >= len(pat) {
				return "", &SyntaxError{msg: `\ at end of pattern`}
			}
			sb.WriteString(regexp.QuoteMeta(string(pat[i])))
		case '[':
			name, err := charClass(pat[i:])
			if err != nil {
				return "", &SyntaxError{msg: "charClass invalid", err: err}
			}
			if name != "" {
				sb.WriteString(name)
				i += len(name) - 1
				break
			}
			if mode&Filenames != 0 {
				for _, c := range pat[i:] {
					if c == ']' {
						break
					} else if c == '/' {
						sb.WriteString("\\[")
						continue writeLoop
					}
				}
			}
			sb.WriteByte(c)
			if i++; i >= len(pat) {
				return "", &SyntaxError{msg: "[ was not matched with a closing ]"}
			}
			switch c = pat[i]; c {
			case '!', '^':
				sb.WriteByte('^')
				if i++; i >= len(pat) {
					return "", &SyntaxError{msg: "[ was not matched with a closing ]"}
				}
			}
			if c = pat[i]; c == ']' {
				sb.WriteByte(']')
				if i++; i >= len(pat) {
					return "", &SyntaxError{msg: "[ was not matched with a closing ]"}
				}
			}
			rangeStart := byte(0)
		loopBracket:
			for ; i < len(pat); i++ {
				c = pat[i]
				sb.WriteByte(c)
				switch c {
				case '\\':
					if i++; i < len(pat) {
						sb.WriteByte(pat[i])
					}
					continue
				case ']':
					break loopBracket
				}
				if rangeStart != 0 && rangeStart > c {
					return "", &SyntaxError{msg: fmt.Sprintf("invalid range: %c-%c", rangeStart, c)}
				}
				if c == '-' {
					rangeStart = pat[i-1]
				} else {
					rangeStart = 0
				}
			}
			if i >= len(pat) {
				return "", &SyntaxError{msg: "[ was not matched with a closing ]"}
			}
		case '{':
			if mode&Braces == 0 {
				sb.WriteString(regexp.QuoteMeta(string(c)))
				break
			}
			innerLevel := 1
			commas := false
		peekBrace:
			for j := i + 1; j < len(pat); j++ {
				switch c := pat[j]; c {
				case '{':
					innerLevel++
				case ',':
					commas = true
				case '\\':
					j++
				case '}':
					if innerLevel--; innerLevel > 0 {
						continue
					}
					if !commas {
						break peekBrace
					}
					closingBraces = append(closingBraces, j)
					sb.WriteString("(?:")
					continue writeLoop
				}
			}
			if match := numRange.FindStringSubmatch(pat[i+1:]); len(match) == 3 {
				start, err1 := strconv.Atoi(match[1])
				end, err2 := strconv.Atoi(match[2])
				if err1 != nil || err2 != nil || start > end {
					return "", &SyntaxError{msg: fmt.Sprintf("invalid range: %q", match[0])}
				}
				// TODO: can we do better here?
				sb.WriteString("(?:")
				for n := start; n <= end; n++ {
					if n > start {
						sb.WriteByte('|')
					}
					fmt.Fprintf(&sb, "%d", n)
				}
				sb.WriteByte(')')
				i += len(match[0])
				break
			}
			sb.WriteString(regexp.QuoteMeta(string(c)))
		case ',':
			if len(closingBraces) == 0 {
				sb.WriteString(regexp.QuoteMeta(string(c)))
			} else {
				sb.WriteByte('|')
			}
		case '}':
			if len(closingBraces) > 0 && closingBraces[len(closingBraces)-1] == i {
				sb.WriteByte(')')
				closingBraces = closingBraces[:len(closingBraces)-1]
			} else {
				sb.WriteString(regexp.QuoteMeta(string(c)))
			}
		default:
			if c > 128 {
				sb.WriteByte(c)
			} else {
				sb.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
	}
	if mode&EntireString != 0 {
		sb.WriteString("$")
	}
	// No `.` metacharacters were used, so don't return the (?s) flag.
	if !dotMeta {
		return sb.String()[4:], nil
	}
	return sb.String(), nil
}

func charClass(s string) (string, error) {
	if strings.HasPrefix(s, "[[.") || strings.HasPrefix(s, "[[=") {
		return "", fmt.Errorf("collating features not available")
	}
	name, ok := strings.CutPrefix(s, "[[:")
	if !ok {
		return "", nil
	}
	name, _, ok = strings.Cut(name, ":]]")
	if !ok {
		return "", fmt.Errorf("[[: was not matched with a closing :]]")
	}
	switch name {
	case "alnum", "alpha", "ascii", "blank", "cntrl", "digit", "graph",
		"lower", "print", "punct", "space", "upper", "word", "xdigit":
	default:
		return "", fmt.Errorf("invalid character class: %q", name)
	}
	return s[:len(name)+6], nil
}

// HasMeta returns whether a string contains any unescaped pattern
// metacharacters: '*', '?', or '['. When the function returns false, the given
// pattern can only match at most one string.
//
// For example, HasMeta(`foo\*bar`) returns false, but HasMeta(`foo*bar`)
// returns true.
//
// This can be useful to avoid extra work, like [Regexp]. Note that this
// function cannot be used to avoid [QuoteMeta], as backslashes are quoted by
// that function but ignored here.
func HasMeta(pat string, mode Mode) bool {
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			i++
		case '*', '?', '[':
			return true
		case '{':
			if mode&Braces != 0 {
				return true
			}
		}
	}
	return false
}

// QuoteMeta returns a string that quotes all pattern metacharacters in the
// given text. The returned string is a pattern that matches the literal text.
//
// For example, QuoteMeta(`foo*bar?`) returns `foo\*bar\?`.
func QuoteMeta(pat string, mode Mode) string {
	needsEscaping := false
loop:
	for _, r := range pat {
		switch r {
		case '{':
			if mode&Braces == 0 {
				continue
			}
			fallthrough
		case '*', '?', '[', '\\':
			needsEscaping = true
			break loop
		}
	}
	if !needsEscaping { // short-cut without a string copy
		return pat
	}
	var sb strings.Builder
	for _, r := range pat {
		switch r {
		case '*', '?', '[', '\\':
			sb.WriteByte('\\')
		case '{':
			if mode&Braces != 0 {
				sb.WriteByte('\\')
			}
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
