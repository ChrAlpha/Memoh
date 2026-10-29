package storefs

import (
	"crypto/sha256"
	"reflect"
	"sync"
)

// conceptLinkUse is a [[slug]] link a concept body contains and the path it
// resolved to ("" when unresolved).
type conceptLinkUse struct {
	slug    string
	relPath string
}

// conceptRender is what a concept file rendered to and everything the
// rendering read: the plan's item, path and title, and how each body link
// resolved.
type conceptRender struct {
	item    MemoryItem
	relPath string
	title   string
	links   []conceptLinkUse
	digest  [sha256.Size]byte
}

func newConceptRender(plan conceptFilePlan, links map[string]conceptLinkTarget) conceptRender {
	content, uses := renderConceptMD(plan, links)
	return conceptRender{
		item:    cloneMemoryItem(plan.Item),
		relPath: plan.RelPath,
		title:   plan.Title,
		links:   uses,
		digest:  sha256.Sum256([]byte(content)),
	}
}

// renders reports whether plan under links renders to the same file.
func (r conceptRender) renders(plan conceptFilePlan, links map[string]conceptLinkTarget) bool {
	if r.relPath != plan.RelPath || r.title != plan.Title || !sameRenderedItem(r.item, plan.Item) {
		return false
	}
	for _, use := range r.links {
		if resolveConceptLink(links, use.slug) != use.relPath {
			return false
		}
	}
	return true
}

// sameRenderedItem compares the item fields a concept file renders.
func sameRenderedItem(a, b MemoryItem) bool {
	return a.ID == b.ID && a.Memory == b.Memory && a.Hash == b.Hash &&
		a.CreatedAt == b.CreatedAt && a.UpdatedAt == b.UpdatedAt &&
		reflect.DeepEqual(a.Metadata, b.Metadata)
}

// conceptRenders remembers each bot's last concept renderings, so a sync
// renders only the concepts whose inputs changed. A remembered rendering is
// only compared with what is on disk, never written as is.
type conceptRenders struct {
	mu   sync.Mutex
	bots map[string]map[string]conceptRender
}

func (c *conceptRenders) get(botID string) map[string]conceptRender {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bots[botID]
}

func (c *conceptRenders) set(botID string, renders map[string]conceptRender) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if renders == nil {
		delete(c.bots, botID)
		return
	}
	if c.bots == nil {
		c.bots = make(map[string]map[string]conceptRender)
	}
	c.bots[botID] = renders
}
