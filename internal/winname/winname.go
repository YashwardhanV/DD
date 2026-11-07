// Package winname knows which file names Windows can store, and how to turn
// any name into one it can.
//
// Phones, Linux and macOS allow names Windows rejects: "meeting 10:30.txt",
// "what?.jpg", "CON.txt", "notes." (a trailing dot). Copied as-is, such a name
// fails - or worse: on NTFS "a:b" silently writes an alternate data stream of a
// file called "a" instead of a file. The rules here (Windows' own):
//
//   - the characters < > : " / \ | ? * and code points 0 to 31 are forbidden;
//   - a name may not end with a space or a dot;
//   - CON PRN AUX NUL COM1-COM9 LPT1-LPT9 (also with superscript digits ¹ ² ³)
//     are reserved, with or without an extension, in any letter case;
//   - a name is at most 255 UTF-16 units long.
//
// Fix maps a bad name to a valid one that still looks like the original:
// each forbidden character becomes a similar-looking full-width character
// (: becomes ：), control characters become U+FFFD, a trailing dot or space
// becomes _, a reserved name gets _ after its stem (CON.txt becomes CON_.txt)
// and an over-long name is cut, keeping its extension, and marked with ~ and
// 8 hex digits of the hash of the original name, so two long names that start
// alike stay different.
package winname

import (
	"encoding/hex"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zeebo/xxh3"
)

// MaxUnits is the longest name NTFS, exFAT and FAT32 store, in UTF-16 units.
const MaxUnits = 255

// fullWidth maps each forbidden character to its full-width look-alike.
var fullWidth = map[rune]rune{
	':':  '：',
	'?':  '？',
	'*':  '＊',
	'<':  '＜',
	'>':  '＞',
	'|':  '｜',
	'"':  '＂',
	'\\': '＼',
	'/':  '／',
}

// Problem says what is wrong with a name.
type Problem int

const (
	OK          Problem = iota
	BadChar             // a forbidden or control character
	TrailingDot         // ends with a dot or a space
	Reserved            // a device name such as CON or LPT1
	TooLong             // more than MaxUnits UTF-16 units
	Empty               // "" (or only dots and spaces)
)

func (p Problem) String() string {
	switch p {
	case BadChar:
		return "has characters Windows doesn't allow"
	case TrailingDot:
		return "ends with a dot or a space"
	case Reserved:
		return "is a name Windows reserves for devices"
	case TooLong:
		return "is longer than 255 characters"
	case Empty:
		return "is empty"
	}
	return "is fine"
}

// Check returns the first problem with name, or OK.
func Check(name string) Problem {
	if name == "" || name == "." || name == ".." {
		return Empty
	}
	for _, r := range name {
		if r < 0x20 || fullWidth[r] != 0 || r == utf8.RuneError && !utf8.ValidString(name) {
			return BadChar
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return TrailingDot
	}
	if reserved(name) {
		return Reserved
	}
	if Units(name) > MaxUnits {
		return TooLong
	}
	return OK
}

// Valid reports whether Windows can store name as it is.
func Valid(name string) bool { return Check(name) == OK }

// Units is the length of s in UTF-16 units (what the 255 limit counts).
func Units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// reserved reports whether name's stem (the part before the first dot, with
// trailing spaces removed) is a device name.
func reserved(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) >= 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		d := stem[3:]
		switch d {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// Fix returns a name Windows can store and whether it differs from name. A
// valid name is always returned unchanged.
func Fix(name string) (string, bool) {
	if Valid(name) {
		return name, false
	}
	var b strings.Builder
	for i, r := range name {
		switch {
		case r == utf8.RuneError && !validAt(name, i):
			b.WriteRune('�')
		case r < 0x20:
			b.WriteRune('�')
		case fullWidth[r] != 0:
			b.WriteRune(fullWidth[r])
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	// Trailing dots and spaces: Windows would silently strip them, so make
	// them visible characters.
	if t := strings.TrimRight(s, ". "); t != s {
		s = t + strings.Repeat("_", len(s)-len(t))
	}
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	if reserved(s) {
		stem, ext := s, ""
		if i := strings.IndexByte(s, '.'); i >= 0 {
			stem, ext = s[:i], s[i:]
		}
		s = strings.TrimRight(stem, " ") + "_" + ext
	}
	if Units(s) > MaxUnits {
		s = shorten(s, name)
	}
	return s, s != name
}

// validAt reports whether the byte at i starts a valid UTF-8 sequence (a real
// U+FFFD in the name is fine; a broken byte is not).
func validAt(s string, i int) bool {
	r, size := utf8.DecodeRuneInString(s[i:])
	return !(r == utf8.RuneError && size <= 1)
}

// Shorten cuts name to fit MaxUnits, keeping its extension, and marks it with
// "~" and 8 hex digits of the XXH3 hash of the original name.
func shorten(s, original string) string {
	sum := xxh3.HashString(original)
	var tag [8]byte
	for i := 0; i < 8; i++ {
		tag[i] = byte(sum >> (56 - 8*i))
	}
	mark := "~" + hex.EncodeToString(tag[:4])
	ext := ""
	if i := strings.LastIndexByte(s, '.'); i > 0 && Units(s[i:]) <= 32 {
		ext = s[i:]
		s = s[:i]
	}
	room := MaxUnits - Units(mark) - Units(ext)
	var b strings.Builder
	n := 0
	for _, r := range s {
		u := utf16.RuneLen(r)
		if n+u > room {
			break
		}
		b.WriteRune(r)
		n += u
	}
	return strings.TrimRight(b.String(), ". ") + mark + ext
}

// Fold is the form used to decide whether two names are "the same" on a
// case-insensitive file system. Windows uses its own upper-case table, which
// matches Unicode simple upper-casing for practically every name.
func Fold(name string) string { return strings.ToUpper(name) }

// CaseVariant returns name with " (case N)" inserted before the extension:
// photo.jpg with n=2 becomes "photo (case 2).jpg".
func CaseVariant(name string, n int) string {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	return stem + " (case " + itoa(n) + ")" + ext
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
