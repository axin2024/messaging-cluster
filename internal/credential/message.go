package credential

import (
	"strings"
	"unicode/utf8"
)

func ValidMessage(clientID, body string, attachments []uint64) bool {
	if !ValidKey(clientID) || len(body) > 4000 || !utf8.ValidString(body) || len(attachments) > 8 ||
		(len(attachments) == 0 && strings.TrimSpace(body) == "") {
		return false
	}
	seen := make(map[uint64]struct{}, len(attachments))
	for _, attachment := range attachments {
		if _, duplicate := seen[attachment]; attachment == 0 || duplicate {
			return false
		}
		seen[attachment] = struct{}{}
	}
	return true
}
