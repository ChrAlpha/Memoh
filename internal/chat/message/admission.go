package message

// Admission records why a channel row was kept out of the model context at
// the door. Rows carry it in metadata under the key SetAdmission writes. The
// SQL readers that serve a model match that key by name.
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
// reach a model: as context, as memory-query text, as compaction input, or in
// the results of the history tools.
// Default false, so chat and discuss agree. Changing it is a product decision,
// not a per-call switch. Every reader whose output reaches a model filters in
// SQL, so refused rows neither use up a byte budget or row limit nor appear in
// a result; the readers receive it as the include_unadmitted argument.
const ContextIncludesUnadmittedPassive = false

// SetAdmission stores the admission outcome in a metadata map for a row being
// written.
func SetAdmission(metadata map[string]any, admission Admission) {
	metadata[metadataKeyAdmission] = string(admission)
}
