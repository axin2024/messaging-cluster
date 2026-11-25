package credential

import (
	"strings"
	"unicode/utf8"
)

func ValidMessage(clientID, body string) bool {
	return ValidKey(clientID) && len(body) <= 4000 && strings.TrimSpace(body) != "" && utf8.ValidString(body)
}
