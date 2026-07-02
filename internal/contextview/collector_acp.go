package contextview

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/prune"
)

const (
	acpSectionsCollectorName = "acp_sections"
	acpSectionsSource        = "acp_context"
)

// ACPSection is one structurally built block of the ACP context resource:
// the preamble or a "## Title" section, fully rendered by its source.
type ACPSection struct {
	ID   string
	Text string
}

type ACPSectionsConfig struct {
	Sections []ACPSection
}

// ACPSectionsCollector turns structurally assembled ACP context sections into
// fragments. Sections arrive pre-rendered from their sources (runtime
// metadata, conversation metadata, workspace files with source-local budget
// pruning), so no markdown re-parsing ever happens.
type ACPSectionsCollector struct{}

func (*ACPSectionsCollector) Name() string {
	return acpSectionsCollectorName
}

func (*ACPSectionsCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := acpSectionsConfig(req.Config)
	if err != nil {
		return nil, err
	}
	frags := make([]contextfrag.ContextFrag, 0, len(cfg.Sections))
	for i, section := range cfg.Sections {
		text := strings.TrimSpace(section.Text)
		if text == "" {
			continue
		}
		id := strings.TrimSpace(section.ID)
		if id == "" {
			id = fmt.Sprintf("acp.section.%03d", i)
		}
		frags = append(frags, contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:         id,
			Kind:       contextfrag.KindACPContext,
			Role:       sdk.MessageRoleSystem,
			Slot:       contextfrag.SlotSystem,
			Text:       text,
			Priority:   35,
			CacheClass: contextfrag.CacheDynamic,
			Trust:      contextfrag.TrustSystem,
			Scope:      req.Scope,
			Source:     acpSectionsSource,
			SourceID:   id,
			Collector:  acpSectionsCollectorName,
			Index:      i,
			Render:     contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		}))
	}
	return frags, nil
}

func acpSectionsConfig(config any) (ACPSectionsConfig, error) {
	if config == nil {
		return ACPSectionsConfig{}, nil
	}
	switch cfg := config.(type) {
	case ACPSectionsConfig:
		return cfg, nil
	case *ACPSectionsConfig:
		if cfg == nil {
			return ACPSectionsConfig{}, nil
		}
		return *cfg, nil
	default:
		return ACPSectionsConfig{}, fmt.Errorf("acp_sections config must be ACPSectionsConfig")
	}
}

// FinalizeACPContextMarkdown assembles section blocks into the ACP context
// document exactly like the legacy renderer: every block is followed by a
// blank line and the whole document is bounded by the context budget prune.
func FinalizeACPContextMarkdown(blocks []string) string {
	var sb strings.Builder
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		sb.WriteString(block)
		sb.WriteString("\n\n")
	}
	return prune.PruneWithEdges(sb.String(), "ACP context", prune.Config{
		MaxBytes:  64 * 1024,
		MaxLines:  1600,
		HeadBytes: 48 * 1024,
		TailBytes: 12 * 1024,
		HeadLines: 1200,
		TailLines: 300,
	})
}
