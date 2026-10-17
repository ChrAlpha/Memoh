package runreq

import (
	"github.com/felinics/memoh/internal/slash"
)

// RejectReservedSkillMetadataIfPresent rejects a request that carries reserved skill metadata in attachments or content parts.
func RejectReservedSkillMetadataIfPresent(req ChatRequest) error {
	for _, msg := range req.Messages {
		for _, part := range msg.ContentParts() {
			if err := slash.RejectReservedSkillMetadataValue(part.Metadata); err != nil {
				return err
			}
		}
	}
	for _, att := range req.Attachments {
		if err := slash.RejectReservedSkillMetadataValue(att.Metadata); err != nil {
			return err
		}
	}
	for _, att := range req.ReplyAttachments {
		if err := slash.RejectReservedSkillMetadataValue(att.Metadata); err != nil {
			return err
		}
	}
	return nil
}
