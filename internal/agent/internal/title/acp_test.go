package title

import (
	"testing"

	session "github.com/felinics/memoh/internal/chat/thread"
)

func TestShouldGenerateSessionTitleAllowsACPPlaceholderTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sess session.Thread
		want bool
	}{
		{
			name: "empty title",
			sess: session.Thread{Type: session.TypeChat},
			want: true,
		},
		{
			name: "normal chat existing title",
			sess: session.Thread{Type: session.TypeChat, Title: "Existing"},
			want: false,
		},
		{
			name: "acp display placeholder",
			sess: session.Thread{
				Type:  session.TypeACPAgent,
				Title: "Codex",
				Metadata: map[string]any{
					"acp_agent_id": "codex",
				},
			},
			want: true,
		},
		{
			name: "acp user title",
			sess: session.Thread{
				Type:  session.TypeACPAgent,
				Title: "Real work",
				Metadata: map[string]any{
					"acp_agent_id": "codex",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldGenerateSessionTitle(tt.sess); got != tt.want {
				t.Fatalf("shouldGenerateSessionTitle() = %v, want %v", got, tt.want)
			}
		})
	}
}
