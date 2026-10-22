package message

// Admission records whether a history row may reach a model. The channel
// gates decide it when the row is written and the bot_history_messages
// admission column holds it; its CHECK lists exactly the values below, so a new
// value needs a migration that changes the CHECK. The SQL readers that serve a
// model keep only AdmissionAdmitted rows.
type Admission string

const (
	// AdmissionAdmitted is a row that may reach a model. It is the column's
	// default and the meaning of an unset Admission on a PersistInput.
	AdmissionAdmitted Admission = "admitted"
	// AdmissionRejectedACL marks a row from a sender the access list refused.
	AdmissionRejectedACL Admission = "rejected_acl"
	// AdmissionRejectedRuntimeDriver marks a row refused by the external runtime
	// driver gate (owner-only or participant rules).
	AdmissionRejectedRuntimeDriver Admission = "rejected_runtime_driver"
)

// ContextIncludesUnadmittedPassive decides whether rows refused at admission
// (sender not allowed by ACL, or refused by the external runtime driver gate)
// reach a model: as context, as memory-query text, as compaction input, or in
// the results of the history tools.
// Default false, so chat and discuss agree. Changing it is a product decision,
// not a per-call switch. Every reader whose output reaches a model filters in
// SQL, so refused rows neither use up a byte budget or row limit nor appear in
// a result; the readers receive it as the include_unadmitted argument.
const ContextIncludesUnadmittedPassive = false

// orAdmitted is the admission a row is written with: an unset one is admitted.
func (a Admission) orAdmitted() string {
	if a == "" {
		return string(AdmissionAdmitted)
	}
	return string(a)
}
