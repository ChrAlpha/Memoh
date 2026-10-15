package application

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func (c *agentStepCommitter) committedContextSources(ctx context.Context, count int, origins []int, persisted []messagepkg.Message) ([]contextfrag.ContextRef, error) {
	sources := make([]contextfrag.ContextRef, count)
	if len(origins) != len(persisted) || c.service == nil || c.service.messageService == nil ||
		(c.req.TurnReplacement != nil && !c.replacementFinalized) {
		return sources, nil
	}
	seen := make(map[int]bool, count)
	for i, origin := range origins {
		if origin < 0 || origin >= count {
			continue
		}
		if seen[origin] {
			sources[origin] = contextfrag.ContextRef{}
			continue
		}
		seen[origin] = true
		original := persisted[i]
		loaded, err := c.service.messageService.GetByIDBySession(ctx, original.SessionID, original.ID)
		if err != nil {
			return nil, fmt.Errorf("load committed context source %s: %w", original.ID, err)
		}
		if !sameCommittedSource(original, loaded) {
			continue
		}
		record, err := historyfrag.FromDBMessage(loaded, historyfrag.ScopeFallback{})
		if err != nil {
			return nil, fmt.Errorf("decode committed context source %s: %w", original.ID, err)
		}
		sources[origin] = record.Ref
	}
	return sources, nil
}

func sameCommittedSource(original, loaded messagepkg.Message) bool {
	if !sameCommittedJSON(original.Content, loaded.Content) || !sameCommittedJSON(original.Usage, loaded.Usage) {
		return false
	}
	original.Content, original.Usage = loaded.Content, loaded.Usage
	original.Platform = loaded.Platform
	return historyfrag.DBMessageSourceHash(original) == historyfrag.DBMessageSourceHash(loaded)
}

func sameCommittedJSON(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return normalizeCommittedJSONNumbers(value), err
	}
	a, errA := decode(left)
	b, errB := decode(right)
	return errA == nil && errB == nil && reflect.DeepEqual(a, b)
}

type committedJSONNumber string

func normalizeCommittedJSONNumbers(value any) any {
	switch value := value.(type) {
	case json.Number:
		if number, ok := new(big.Rat).SetString(string(value)); ok {
			return committedJSONNumber(number.RatString())
		}
	case []any:
		for i := range value {
			value[i] = normalizeCommittedJSONNumbers(value[i])
		}
	case map[string]any:
		for key, item := range value {
			value[key] = normalizeCommittedJSONNumbers(item)
		}
	}
	return value
}
