package agenttest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
)

// ModelQueries is an in-memory dbstore.Queries fake for the tests that select
// a chat model. The embedded interface panics on any method the
// test did not expect to be called.
type ModelQueries struct {
	dbstore.Queries

	Models         map[string]sqlc.Model
	Provider       sqlc.Provider
	SessionModelID pgtype.UUID
	UpdatedPrefs   []sqlc.UpdateSessionModelPreferenceParams
	// updatePrefErr simulates a failed preference write (#879): write-back
	// must degrade to a log line, never fail the turn.
	UpdatePrefErr error
	// patchedPrefs records picker compare-and-set writes; the fake always
	// reports one row matched.
	PatchedPrefs []sqlc.CompareAndSetSessionModelPreferenceParams
	// session is the row GetSessionByID serves; the zero value is the "no
	// memory" default every derived fake wants (issue #879). Half-pair tests
	// set it explicitly.
	Session sqlc.BotSession
}

// GetDiscussSession reports no discuss session: every run end asks, and the
// sessions these fakes serve are chat sessions.
func (*ModelQueries) GetDiscussSession(context.Context, sqlc.GetDiscussSessionParams) (sqlc.GetDiscussSessionRow, error) {
	return sqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}

// GetSessionByID returns the configured row (zero row by default: no
// persisted model preference). Kept out of the embedded store so a
// preference read can never panic on the nil interface.
func (f *ModelQueries) GetSessionByID(_ context.Context, id pgtype.UUID) (sqlc.BotSession, error) {
	if f.Session.ID.Valid {
		return f.Session, nil
	}
	return sqlc.BotSession{ID: id}, nil
}

// updatedPrefs records preference writes for the write-back gate tests (#879).
func (f *ModelQueries) UpdateSessionModelPreference(_ context.Context, arg sqlc.UpdateSessionModelPreferenceParams) error {
	if f.UpdatePrefErr != nil {
		return f.UpdatePrefErr
	}
	f.UpdatedPrefs = append(f.UpdatedPrefs, arg)
	return nil
}

func (f *ModelQueries) CompareAndSetSessionModelPreference(_ context.Context, arg sqlc.CompareAndSetSessionModelPreferenceParams) (int64, error) {
	f.PatchedPrefs = append(f.PatchedPrefs, arg)
	return 1, nil
}

func (f *ModelQueries) ListModelsByModelID(_ context.Context, modelID string) ([]sqlc.Model, error) {
	model, ok := f.Models[modelID]
	if !ok {
		return nil, nil
	}
	return []sqlc.Model{model}, nil
}

func (f *ModelQueries) GetModelByID(_ context.Context, id pgtype.UUID) (sqlc.Model, error) {
	for _, model := range f.Models {
		if id.Valid && model.ID == id {
			return model, nil
		}
	}
	return sqlc.Model{}, pgx.ErrNoRows
}

func (f *ModelQueries) GetProviderByID(_ context.Context, id pgtype.UUID) (sqlc.Provider, error) {
	if !id.Valid || id != f.Provider.ID {
		return sqlc.Provider{}, pgx.ErrNoRows
	}
	return f.Provider, nil
}

func (f *ModelQueries) GetLatestSessionModelID(_ context.Context, _ pgtype.UUID) (pgtype.UUID, error) {
	if !f.SessionModelID.Valid {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return f.SessionModelID, nil
}

func ProviderRow(t *testing.T, id string, clientType string, enable bool) sqlc.Provider {
	t.Helper()
	pgID, err := db.ParseUUID(id)
	if err != nil {
		t.Fatalf("parse provider uuid: %v", err)
	}
	return sqlc.Provider{
		ID:         pgID,
		Name:       "provider-" + id,
		ClientType: clientType,
		Enable:     enable,
		Config:     []byte(`{}`),
		Metadata:   []byte(`{}`),
	}
}

func ModelRow(t *testing.T, id string, modelID string, providerID pgtype.UUID, modelType models.ModelType, enable bool) sqlc.Model {
	t.Helper()
	pgID, err := db.ParseUUID(id)
	if err != nil {
		t.Fatalf("parse model uuid: %v", err)
	}
	return sqlc.Model{
		ID:         pgID,
		ModelID:    modelID,
		Name:       pgtype.Text{String: modelID, Valid: true},
		ProviderID: providerID,
		Type:       string(modelType),
		Enable:     enable,
		Config:     []byte(`{}`),
	}
}
