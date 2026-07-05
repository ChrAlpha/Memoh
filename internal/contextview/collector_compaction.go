package contextview

import (
	"context"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/historyfrag"
)

const compactionRecordsCollectorName = "compaction_records"

type CompactionRecordsConfig struct {
	Records []historyfrag.HistoryRecord
}

// CompactionRecordsCollector turns uncompacted history records into fragments
// so compaction candidate selection and prompt rendering run through the same
// build lifecycle as every other intent.
type CompactionRecordsCollector struct{}

func (*CompactionRecordsCollector) Name() string {
	return compactionRecordsCollectorName
}

func (*CompactionRecordsCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := compactionRecordsConfig(req.Config)
	if err != nil {
		return nil, err
	}
	frags := make([]contextfrag.ContextFrag, 0, len(cfg.Records))
	for i, record := range cfg.Records {
		frag := historyfrag.ToFrag(record)
		frag.Provenance.Index = i
		frags = append(frags, frag)
	}
	return frags, nil
}

func compactionRecordsConfig(config any) (CompactionRecordsConfig, error) {
	return collectorConfig[CompactionRecordsConfig](config, "compaction_records config must be CompactionRecordsConfig")
}
