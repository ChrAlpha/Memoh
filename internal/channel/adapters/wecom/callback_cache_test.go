package wecom

import (
	"testing"
	"time"
)

func TestCallbackContextCache_PutGet(t *testing.T) {
	cache := newCallbackContextCache(1 * time.Hour)
	cache.Put("config-a", "m1", callbackContext{ReqID: "r1"})
	got, ok := cache.Get("config-a", "m1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.ReqID != "r1" {
		t.Fatalf("unexpected req id: %q", got.ReqID)
	}
}

func TestCallbackContextCache_Expires(t *testing.T) {
	cache := newCallbackContextCache(1 * time.Second)
	cache.Put("config-a", "m1", callbackContext{
		ReqID:     "r1",
		CreatedAt: time.Now().Add(-2 * time.Second),
	})
	if _, ok := cache.Get("config-a", "m1"); ok {
		t.Fatal("expected cache miss due to expiry")
	}
}

func TestCallbackContextCacheIsScopedByConfig(t *testing.T) {
	cache := newCallbackContextCache(time.Hour)
	cache.Put("config-a", "shared-message", callbackContext{ReqID: "request-a"})
	cache.Put("config-b", "shared-message", callbackContext{ReqID: "request-b"})

	first, ok := cache.Get("config-a", "shared-message")
	if !ok || first.ReqID != "request-a" {
		t.Fatalf("config-a callback = %#v, found=%v", first, ok)
	}
	second, ok := cache.Get("config-b", "shared-message")
	if !ok || second.ReqID != "request-b" {
		t.Fatalf("config-b callback = %#v, found=%v", second, ok)
	}
}

func TestWeComClientCacheKeyIsScopedByConfig(t *testing.T) {
	first := wecomClientCacheKey("config-a", "shared-bot")
	if second := wecomClientCacheKey("config-b", "shared-bot"); second == first {
		t.Fatal("different configs must not share a WeCom client cache key")
	}
}

func TestCallbackContextCacheLatestIsTheNewestCallbackOfTheConversation(t *testing.T) {
	cache := newCallbackContextCache(time.Hour)
	cache.Put("config-a", "m1", callbackContext{ReqID: "r1", ChatID: "chat-1"})
	cache.Put("config-a", "m2", callbackContext{ReqID: "r2", ChatID: "chat-1"})
	cache.Put("config-a", "m3", callbackContext{ReqID: "r3", UserID: "alice"})
	cache.Put("config-b", "m4", callbackContext{ReqID: "r4", ChatID: "chat-1"})

	cases := []struct {
		config, target, want string
	}{
		{"config-a", "chat_id:chat-1", "r2"},
		{"config-a", "user_id:alice", "r3"},
		{"config-b", "chat_id:chat-1", "r4"},
	}
	for _, tc := range cases {
		got, ok := cache.Latest(tc.config, tc.target)
		if !ok || got.ReqID != tc.want {
			t.Errorf("Latest(%q, %q) = %q (found %v), want %q", tc.config, tc.target, got.ReqID, ok, tc.want)
		}
	}
	if _, ok := cache.Latest("config-a", "chat_id:other"); ok {
		t.Error("Latest found a callback for a conversation that has none")
	}
	if _, ok := cache.Latest("config-a", ""); ok {
		t.Error("Latest found a callback for no target")
	}
}

func TestCallbackContextCacheLatestExpires(t *testing.T) {
	cache := newCallbackContextCache(time.Second)
	cache.Put("config-a", "m1", callbackContext{ReqID: "r1", ChatID: "chat-1", CreatedAt: time.Now().Add(-2 * time.Second)})
	if _, ok := cache.Latest("config-a", "chat_id:chat-1"); ok {
		t.Fatal("Latest returned an expired callback")
	}
}
