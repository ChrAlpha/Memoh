package adapters

import "testing"

func TestEncodeSourceRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		sessionID string
		messageID string
		want      string
	}{
		{"session and message", "sess-1", "msg-1", "sess-1/msg-1"},
		{"bare message", "", "msg-1", "msg-1"},
		{"trims whitespace", " sess-1 ", " msg-1 ", "sess-1/msg-1"},
		{"empty message", "sess-1", "", ""},
		{"empty both", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := EncodeSourceRef(tc.sessionID, tc.messageID); got != tc.want {
				t.Fatalf("EncodeSourceRef(%q, %q) = %q, want %q", tc.sessionID, tc.messageID, got, tc.want)
			}
		})
	}
}

func TestParseSourceRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		ref         string
		wantSession string
		wantMessage string
	}{
		{"composite", "sess-1/msg-1", "sess-1", "msg-1"},
		{"bare message id", "msg-1", "", "msg-1"},
		{"trims whitespace", " sess-1/msg-1 ", "sess-1", "msg-1"},
		{"empty", "", "", ""},
		{"only separator", "/", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSession, gotMessage := ParseSourceRef(tc.ref)
			if gotSession != tc.wantSession || gotMessage != tc.wantMessage {
				t.Fatalf("ParseSourceRef(%q) = (%q, %q), want (%q, %q)", tc.ref, gotSession, gotMessage, tc.wantSession, tc.wantMessage)
			}
		})
	}
}

func TestEncodeParseSourceRefRoundTrip(t *testing.T) {
	t.Parallel()
	ref := EncodeSourceRef("0d9c1a2b-3e4f-4a5b-8c6d-7e8f9a0b1c2d", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")
	session, message := ParseSourceRef(ref)
	if session != "0d9c1a2b-3e4f-4a5b-8c6d-7e8f9a0b1c2d" || message != "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d" {
		t.Fatalf("round trip failed: got (%q, %q)", session, message)
	}
}
