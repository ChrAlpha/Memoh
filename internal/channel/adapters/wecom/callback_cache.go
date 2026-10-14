package wecom

import (
	"strings"
	"sync"
	"time"
)

type callbackContext struct {
	ReqID       string
	ResponseURL string
	ChatID      string
	UserID      string
	CreatedAt   time.Time
}

// callbackContextTTL is how long after a message callback WeCom accepts a reply
// to that conversation ("收到消息回调后，24 小时内可以往该会话回复消息", long
// connection guide, developer.work.weixin.qq.com/document/path/101463).
const callbackContextTTL = 24 * time.Hour

type callbackContextCache struct {
	mu    sync.RWMutex
	items map[string]callbackContext
	// latest maps a conversation to the key of its newest callback in items.
	latest map[string]string
	ttl    time.Duration
}

func newCallbackContextCache(ttl time.Duration) *callbackContextCache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &callbackContextCache{
		items:  make(map[string]callbackContext),
		latest: make(map[string]string),
		ttl:    ttl,
	}
}

func (c *callbackContextCache) Put(configID, messageID string, ctx callbackContext) {
	key := callbackContextCacheKey(configID, messageID)
	if key == "" {
		return
	}
	if ctx.CreatedAt.IsZero() {
		ctx.CreatedAt = time.Now().UTC()
	}
	c.mu.Lock()
	c.items[key] = ctx
	if conversation := callbackConversationKey(configID, resolveDeliveryTarget(ctx.ChatID, ctx.UserID)); conversation != "" {
		c.latest[conversation] = key
	}
	c.gcLocked()
	c.mu.Unlock()
}

func (c *callbackContextCache) Get(configID, messageID string) (callbackContext, bool) {
	key := callbackContextCacheKey(configID, messageID)
	if key == "" {
		return callbackContext{}, false
	}
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return callbackContext{}, false
	}
	if time.Since(item.CreatedAt) > c.ttl {
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		return callbackContext{}, false
	}
	return item, true
}

// Latest returns the newest callback of the conversation target names. Media
// can only be sent as a reply to a callback, and a message that carries media
// without naming the callback it answers is delivered with this one.
func (c *callbackContextCache) Latest(configID, target string) (callbackContext, bool) {
	conversation := callbackConversationKey(configID, target)
	if conversation == "" {
		return callbackContext{}, false
	}
	c.mu.RLock()
	key := c.latest[conversation]
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || time.Since(item.CreatedAt) > c.ttl {
		return callbackContext{}, false
	}
	return item, true
}

func callbackConversationKey(configID, target string) string {
	configID = strings.TrimSpace(configID)
	target = strings.TrimSpace(target)
	if configID == "" || target == "" {
		return ""
	}
	return configID + "\x00" + target
}

func callbackContextCacheKey(configID, messageID string) string {
	configID = strings.TrimSpace(configID)
	messageID = strings.TrimSpace(messageID)
	if configID == "" || messageID == "" {
		return ""
	}
	return configID + "\x00" + messageID
}

func (c *callbackContextCache) gcLocked() {
	if len(c.items) < 512 {
		return
	}
	now := time.Now().UTC()
	for key, item := range c.items {
		if now.Sub(item.CreatedAt) > c.ttl {
			delete(c.items, key)
		}
	}
	for conversation, key := range c.latest {
		if _, ok := c.items[key]; !ok {
			delete(c.latest, conversation)
		}
	}
}
