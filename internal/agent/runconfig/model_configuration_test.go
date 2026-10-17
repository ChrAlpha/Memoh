package runconfig

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/rpc"
	"github.com/felinics/memoh/internal/settings"
)

type missingModelQueries struct{ agenttest.ModelQueries }

func (*missingModelQueries) GetSettingsByBotID(context.Context, pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{}, nil
}

func (*missingModelQueries) GetBotByID(context.Context, pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return sqlc.GetBotByIDRow{}, nil
}

func TestBuildBaseRunConfigMissingModelPreservesPublicCode(t *testing.T) {
	queries := &missingModelQueries{}
	logger := slog.New(slog.DiscardHandler)
	svc := newBuilder(func(d *Deps) {
		d.Queries = queries
		d.Models = models.NewService(logger, queries)
		d.Credentials = providers.NewService(nil, queries, "")
		d.Bot = botenv.New(botenv.Deps{Logger: logger, Queries: queries, Settings: settings.NewService(logger, queries, nil, nil)})
	})
	_, _, _, err := svc.buildBase(t.Context(), baseParams{BotID: "00000000-0000-0000-0000-000000000001"})
	const want apperror.Code = "agent.chat_model_not_configured"
	if got := outcome.PublicFailureCode(err); got != want {
		t.Fatalf("startup code = %q, want %q (error: %v)", got, want, err)
	}
	wire := rpc.AnswerStatus(t.Context(), fmt.Errorf("SECRET startup context: %w", err))
	if got := apperror.CodeOf(rpc.DecodeAppError(wire)); got != want {
		t.Fatalf("RPC code = %q, want %q", got, want)
	}
	if strings.Contains(wire.Error(), "SECRET") {
		t.Fatalf("private diagnostic crossed the RPC boundary: %v", wire)
	}
}
