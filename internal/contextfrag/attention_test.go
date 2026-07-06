package contextfrag

import (
	"reflect"
	"testing"

	"github.com/memohai/memoh/internal/conversation"
)

func TestDeriveAttention(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   AttentionDerivation
		want []AttentionReason
	}{
		{
			name: "direct type",
			in:   AttentionDerivation{ConversationType: "direct", UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionDirect},
		},
		{
			name: "private normalizes to direct",
			in:   AttentionDerivation{ConversationType: "private", UnknownTypeKind: conversation.KindGroup},
			want: []AttentionReason{AttentionDirect},
		},
		{
			name: "group without flags is passive",
			in:   AttentionDerivation{ConversationType: "group", UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionPassive},
		},
		{
			name: "thread without flags is passive",
			in:   AttentionDerivation{ConversationType: "thread", UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionPassive},
		},
		{
			name: "group mention suppresses passive",
			in:   AttentionDerivation{ConversationType: "group", MentionsBot: true, UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionMention},
		},
		{
			name: "direct keeps mention and reply order",
			in:   AttentionDerivation{ConversationType: "direct", MentionsBot: true, RepliesToBot: true, UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionMention, AttentionReply, AttentionDirect},
		},
		{
			name: "empty type with direct default",
			in:   AttentionDerivation{ConversationType: "", UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionDirect},
		},
		{
			name: "empty type with group default is passive",
			in:   AttentionDerivation{ConversationType: "", UnknownTypeKind: conversation.KindGroup},
			want: []AttentionReason{AttentionPassive},
		},
		{
			name: "empty type with group default keeps mention only",
			in:   AttentionDerivation{ConversationType: "", MentionsBot: true, UnknownTypeKind: conversation.KindGroup},
			want: []AttentionReason{AttentionMention},
		},
		{
			name: "unrecognized type falls back to passive under either default",
			in:   AttentionDerivation{ConversationType: "channel", UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionPassive},
		},
		{
			name: "leading reason gates passive",
			in:   AttentionDerivation{ConversationType: "group", UnknownTypeKind: conversation.KindDirect, Leading: []AttentionReason{AttentionSchedule}},
			want: []AttentionReason{AttentionSchedule},
		},
		{
			name: "trailing reason gates passive",
			in:   AttentionDerivation{ConversationType: "group", UnknownTypeKind: conversation.KindDirect, Trailing: []AttentionReason{AttentionCommand}},
			want: []AttentionReason{AttentionCommand},
		},
		{
			name: "full ordering leading mention reply trailing type",
			in: AttentionDerivation{
				ConversationType: "direct",
				MentionsBot:      true,
				RepliesToBot:     true,
				UnknownTypeKind:  conversation.KindDirect,
				Leading:          []AttentionReason{AttentionHeartbeat},
				Trailing:         []AttentionReason{AttentionCommand},
			},
			want: []AttentionReason{AttentionHeartbeat, AttentionMention, AttentionReply, AttentionCommand, AttentionDirect},
		},
		{
			name: "deduplicates repeated reasons",
			in: AttentionDerivation{
				ConversationType: "group",
				MentionsBot:      true,
				UnknownTypeKind:  conversation.KindDirect,
				Leading:          []AttentionReason{AttentionMention},
			},
			want: []AttentionReason{AttentionMention},
		},
		{
			name: "type is trimmed and case-insensitive",
			in:   AttentionDerivation{ConversationType: " Group ", UnknownTypeKind: conversation.KindDirect},
			want: []AttentionReason{AttentionPassive},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DeriveAttention(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DeriveAttention(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
