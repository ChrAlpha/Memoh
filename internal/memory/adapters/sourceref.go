package adapters

import "strings"

const sourceRefSeparator = "/"

// EncodeSourceRef builds a "<sessionID>/<messageID>" source ref. The session
// part is optional so bare message IDs stay valid refs.
func EncodeSourceRef(sessionID, messageID string) string {
	sessionID = strings.TrimSpace(sessionID)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	if sessionID == "" {
		return messageID
	}
	return sessionID + sourceRefSeparator + messageID
}

// ParseSourceRef splits a source ref into its session and message parts. A ref
// without a separator is treated as a bare message ID.
func ParseSourceRef(ref string) (sessionID, messageID string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	session, message, found := strings.Cut(ref, sourceRefSeparator)
	if !found {
		return "", ref
	}
	return strings.TrimSpace(session), strings.TrimSpace(message)
}
