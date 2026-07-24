package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

type AuditManifestPayload struct {
	Manifest      contextfrag.Manifest
	Intent        contextfrag.Intent
	Scope         contextfrag.Scope
	SelectedCount int
	PlacementPlan PlacementPlan
	ContentHash   string
}

type AuditManifestRenderer struct{}

func (*AuditManifestRenderer) Target() contextfrag.RenderTarget {
	return contextfrag.RenderAuditManifest
}

func (*AuditManifestRenderer) Render(_ context.Context, input RenderInput) (RenderedPayload, error) {
	manifest := contextfrag.BuildManifest(input.Selected)
	if input.Manifest != nil {
		manifest = *input.Manifest
	}
	manifest.View = input.Intent.ManifestView()
	payload := &AuditManifestPayload{
		Manifest:      manifest,
		Intent:        input.Intent,
		Scope:         input.Scope,
		SelectedCount: len(input.Selected),
		PlacementPlan: input.Placement,
	}
	hash, err := auditManifestPayloadHash(payload)
	if err != nil {
		return RenderedPayload{}, err
	}
	payload.ContentHash = hash
	return RenderedPayload{
		Target:      contextfrag.RenderAuditManifest,
		ContentHash: hash,
		Data:        payload,
	}, nil
}

func auditManifestPayloadHash(payload *AuditManifestPayload) (string, error) {
	data, err := json.Marshal(auditManifestHashPayload{
		Manifest:      payload.Manifest,
		Intent:        payload.Intent,
		Scope:         payload.Scope,
		SelectedCount: payload.SelectedCount,
		PlacementPlan: auditPlacementPlanSnapshot(payload.PlacementPlan),
	})
	if err != nil {
		return "", fmt.Errorf("marshal audit manifest payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type auditManifestHashPayload struct {
	Manifest      contextfrag.Manifest `json:"manifest"`
	Intent        contextfrag.Intent   `json:"intent"`
	Scope         contextfrag.Scope    `json:"scope"`
	SelectedCount int                  `json:"selected_count"`
	PlacementPlan auditPlacementPlan   `json:"placement_plan"`
}

type auditPlacementPlan struct {
	StablePrefixHash   string               `json:"stable_prefix_hash"`
	FirstVolatileIndex int                  `json:"first_volatile_index"`
	Items              []auditPlacementItem `json:"items"`
}

type auditPlacementItem struct {
	FragID    string                 `json:"frag_id"`
	Slot      contextfrag.Slot       `json:"slot"`
	Position  int                    `json:"position"`
	CacheHint contextfrag.CacheClass `json:"cache_hint"`
	Ref       contextfrag.ContextRef `json:"ref"`
}

func auditPlacementPlanSnapshot(plan PlacementPlan) auditPlacementPlan {
	items := make([]auditPlacementItem, 0, len(plan.Items))
	for _, item := range plan.Items {
		items = append(items, auditPlacementItem(item))
	}
	return auditPlacementPlan{
		StablePrefixHash:   plan.StablePrefixHash,
		FirstVolatileIndex: plan.FirstVolatileIndex,
		Items:              items,
	}
}
