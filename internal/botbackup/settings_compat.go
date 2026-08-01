package botbackup

import "github.com/memohai/memoh/internal/settings"

func decodeBackupSettings(raw []byte) (settings.Settings, error) {
	var cfg settings.Settings
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := unmarshalJSON(raw, &cfg); err != nil {
		return settings.Settings{}, err
	}
	if cfg.CompactionTargetPercent != nil || cfg.CompactionThreshold <= 0 {
		return cfg, nil
	}
	var legacy struct {
		CompactionRatio *int `json:"compaction_ratio"`
	}
	if err := unmarshalJSON(raw, &legacy); err != nil {
		return settings.Settings{}, err
	}
	if legacy.CompactionRatio == nil {
		return cfg, nil
	}
	target := 100 - *legacy.CompactionRatio
	if target >= 1 && target <= 99 {
		cfg.CompactionTargetPercent = &target
	}
	return cfg, nil
}
