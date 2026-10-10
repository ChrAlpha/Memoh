package sessionfacts

import "testing"

func TestPresetPerMode(t *testing.T) {
	t.Parallel()

	chat := Facts{ReplyDelivery: ReplyStream, TriggerScope: TriggerAddressedMessage, DecisionMode: DecisionPark, ExternalRuntimeDriver: RuntimeDriverOwnerOnly}
	discuss := Facts{ReplyDelivery: ReplyExplicit, TriggerScope: TriggerUncoveredEvents, DecisionMode: DecisionAutoReject, ExternalRuntimeDriver: RuntimeDriverParticipants}
	background := Facts{ReplyDelivery: ReplyStream, TriggerScope: TriggerAddressedMessage, DecisionMode: DecisionAutoReject, ExternalRuntimeDriver: RuntimeDriverOwnerOnly}

	cases := map[string]Facts{
		"chat":      chat,
		"":          chat,
		"acp_agent": chat,
		"discuss":   discuss,
		"schedule":  background,
		"subagent":  background,
	}
	for mode, want := range cases {
		got := Preset(mode)
		if got != want {
			t.Fatalf("Preset(%q) = %+v, want %+v", mode, got, want)
		}
		if !got.Valid() {
			t.Fatalf("Preset(%q) is not valid", mode)
		}
	}
}

func TestValidRejectsUnknownValues(t *testing.T) {
	t.Parallel()

	if (Facts{}).Valid() {
		t.Fatal("empty facts reported valid")
	}
	f := Preset("chat")
	f.DecisionMode = "later"
	if f.Valid() {
		t.Fatal("unknown decision mode reported valid")
	}
	if Audience("").Valid() || !AudienceShared.Valid() || !AudiencePersonal.Valid() {
		t.Fatal("audience validity is wrong")
	}
}
