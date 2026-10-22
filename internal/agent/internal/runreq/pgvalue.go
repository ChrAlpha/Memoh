package runreq

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// PGInt64 is the value of v, or 0 when it is NULL.
func PGInt64(value pgtype.Int8) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

// PGUUIDString is the canonical text of id, or "" when it is NULL.
func PGUUIDString(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	parsed, err := uuid.FromBytes(value.Bytes[:])
	if err != nil {
		return ""
	}
	return parsed.String()
}
