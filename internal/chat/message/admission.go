package message

import "encoding/json"

// Admission records why a channel row was kept out of the model context at
// the door. Rows carry it in metadata; this type is the only reader and writer
// of that key.
type Admission string

const (
	// AdmissionRejectedACL marks a row from a sender the access list refused.
	AdmissionRejectedACL Admission = "rejected_acl"
	// AdmissionRejectedRuntimeDriver marks a row refused by the external runtime
	// driver gate (owner-only or participant rules).
	AdmissionRejectedRuntimeDriver Admission = "rejected_runtime_driver"
)

const metadataKeyAdmission = "admission"

// ContextIncludesUnadmittedPassive decides whether rows refused at admission
// (sender not allowed by ACL, or refused by the external runtime driver gate)
// reach a model: as context, as memory-query text, or as compaction input.
// Default false, so chat and discuss agree. Changing it is a product decision,
// not a per-call switch. Row-level readers test Message.Admission against it;
// SQL readers receive it as the include_unadmitted argument and match the same
// metadata key.
const ContextIncludesUnadmittedPassive = false

// SetAdmission stores the admission outcome in a metadata map for a row being
// written.
func SetAdmission(metadata map[string]any, admission Admission) {
	metadata[metadataKeyAdmission] = string(admission)
}

// Admission returns the admission outcome recorded on the row, or the empty
// value when the row was admitted normally. History loads leave metadata
// undecoded, so the key is read from the raw JSON.
func (m Message) Admission() Admission {
	if len(m.RawMetadata) > 0 {
		var probe struct {
			Admission string `json:"admission"`
		}
		if err := json.Unmarshal(m.RawMetadata, &probe); err != nil {
			return ""
		}
		return Admission(probe.Admission)
	}
	value, _ := m.Metadata[metadataKeyAdmission].(string)
	return Admission(value)
}
