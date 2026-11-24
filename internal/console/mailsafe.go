package console

// What of a member's free text may go into Wakeshift's email. An
// organization's name is whatever its owner typed, so an email that quotes
// it could carry a phisher's link or address ("Acme: verify at
// evil.example") under Wakeshift's name, and mail clients turn such text
// into links. So the email names people by their email address, keeps every
// subject fixed, and quotes an organization's name only when it holds
// nothing that reads as a URL, a domain or an address (mailName).

import (
	"regexp"
	"strings"
	"unicode"
)

// dotLike are characters that read as a dot, which some mail clients link
// as one: each counts as a dot when checking a name.
var dotLike = strings.NewReplacer("。", ".", "．", ".", "｡", ".", "․", ".", "﹒", ".", "‧", ".", "·", ".")

// linkLike matches text a mail client may turn into a link, or a reader
// take for one: a URL's scheme or slashes (https://, //), a scheme that
// needs none (mailto:, tel:), www., an address (@), or a domain (a label, a
// dot, then letters).
var linkLike = regexp.MustCompile(`(?i)//|\b(mailto|tel|sms|callto|javascript|data|file):|www\.|@|[\p{L}\p{N}-]\.[\p{L}]{2,}`)

// mailName is name as an email may quote it: on one line, at most 100
// characters, or "" when it reads as a link (linkLike) or holds characters
// that aren't letters, digits, punctuation or spaces (controls, invisible
// formatting, bidirectional overrides). The caller says something generic
// instead ("an organization").
func mailName(name string) string {
	name = oneLine(name)
	if name == "" || len([]rune(name)) > 100 {
		return ""
	}
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || r == ' ') || unicode.Is(unicode.Cf, r) {
			return ""
		}
	}
	if linkLike.MatchString(dotLike.Replace(name)) {
		return ""
	}
	return name
}
