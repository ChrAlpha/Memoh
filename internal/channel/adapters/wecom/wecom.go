package wecom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/felinics/memoh/internal/channel"
)

const Type channel.ChannelType = "wecom"

type wsClientFactory func(opts WSClientOptions) *WSClient

type WeComAdapter struct {
	logger *slog.Logger

	mu      sync.RWMutex
	clients map[string]*WSClient
	http    *HTTPClient
	cache   *callbackContextCache

	newWSClient wsClientFactory
}

func NewWeComAdapter(log *slog.Logger) *WeComAdapter {
	if log == nil {
		log = slog.Default()
	}
	return &WeComAdapter{
		logger:      log.With(slog.String("adapter", "wecom")),
		clients:     make(map[string]*WSClient),
		http:        NewHTTPClient(HTTPClientOptions{Logger: log}),
		cache:       newCallbackContextCache(callbackContextTTL),
		newWSClient: NewWSClient,
	}
}

func (*WeComAdapter) Type() channel.ChannelType { return Type }

func (*WeComAdapter) Descriptor() channel.Descriptor {
	return channel.Descriptor{
		Type:        Type,
		DisplayName: "WeCom",
		Capabilities: channel.ChannelCapabilities{
			Text:           true,
			Markdown:       true,
			Attachments:    true,
			Media:          true,
			Reply:          true,
			Streaming:      true,
			BlockStreaming: true,
			ChatTypes:      []string{channel.ConversationTypePrivate, channel.ConversationTypeGroup},
			// A reply is one stream message, refreshed under one stream id until
			// finish (aibot_respond_msg, long connection guide).
			Preview: channel.PreviewNativeStream,
			// Replies to a callback are accepted for 24 hours. After that the
			// bot can still push markdown to the conversation with
			// aibot_send_msg, once the user has written to the bot.
			ReplyWindow:   callbackContextTTL,
			ProactiveSend: true,
		},
		ConfigSchema: channel.ConfigSchema{
			Version: 1,
			Fields: map[string]channel.FieldSchema{
				"botId":               {Type: channel.FieldString, Required: true, Title: "Bot ID"},
				"secret":              {Type: channel.FieldSecret, Required: true, Title: "Secret"},
				"wsUrl":               {Type: channel.FieldString, Title: "WebSocket URL", Example: defaultWSURL},
				"heartbeatSeconds":    {Type: channel.FieldNumber, Title: "Heartbeat Seconds"},
				"ackTimeoutSeconds":   {Type: channel.FieldNumber, Title: "Ack Timeout Seconds"},
				"writeTimeoutSeconds": {Type: channel.FieldNumber, Title: "Write Timeout Seconds"},
				"readTimeoutSeconds":  {Type: channel.FieldNumber, Title: "Read Timeout Seconds"},
			},
		},
		UserConfigSchema: channel.ConfigSchema{
			Version: 1,
			Fields: map[string]channel.FieldSchema{
				"chat_id": {Type: channel.FieldString},
				"user_id": {Type: channel.FieldString},
			},
		},
		TargetSpec: channel.TargetSpec{
			Format: "chat_id:xxx | user_id:xxx",
			Hints: []channel.TargetHint{
				{Label: "Chat ID", Example: "chat_id:work_abc"},
				{Label: "User ID", Example: "user_id:zhangsan"},
			},
		},
	}
}

func (*WeComAdapter) NormalizeConfig(raw map[string]any) (map[string]any, error) {
	return normalizeConfig(raw)
}

func (*WeComAdapter) NormalizeUserConfig(raw map[string]any) (map[string]any, error) {
	return normalizeUserConfig(raw)
}

func (*WeComAdapter) NormalizeTarget(raw string) string { return normalizeTarget(raw) }

func (*WeComAdapter) ResolveTarget(userConfig map[string]any) (string, error) {
	return resolveTarget(userConfig)
}

func (*WeComAdapter) MatchBinding(config map[string]any, criteria channel.BindingCriteria) bool {
	return matchBinding(config, criteria)
}

func (*WeComAdapter) BuildUserConfig(identity channel.Identity) map[string]any {
	return buildUserConfig(identity)
}

func (*WeComAdapter) DiscoverSelf(ctx context.Context, credentials map[string]any) (map[string]any, string, error) {
	_ = ctx
	cfg, err := parseConfig(credentials)
	if err != nil {
		return nil, "", err
	}
	externalID := strings.TrimSpace(cfg.BotID)
	identity := map[string]any{
		"bot_id":   externalID,
		"aibot_id": externalID,
	}
	return identity, externalID, nil
}

func (a *WeComAdapter) Connect(ctx context.Context, cfg channel.ChannelConfig, handler channel.InboundHandler) (channel.Connection, error) {
	parsed, err := parseConfig(cfg.Credentials)
	if err != nil {
		return nil, err
	}
	client := a.newWSClient(WSClientOptions{
		URL:                parsed.WSURL,
		Logger:             a.logger,
		HeartbeatInterval:  time.Duration(secondsOrDefault(parsed.HeartbeatSeconds, 30)) * time.Second,
		AckTimeout:         time.Duration(secondsOrDefault(parsed.AckTimeoutSeconds, 8)) * time.Second,
		WriteTimeout:       time.Duration(secondsOrDefault(parsed.WriteTimeoutSeconds, 8)) * time.Second,
		ReadTimeout:        time.Duration(secondsOrDefault(parsed.ReadTimeoutSeconds, 70)) * time.Second,
		ReconnectBaseDelay: 1 * time.Second,
		ReconnectMaxDelay:  30 * time.Second,
	})

	key := wecomClientCacheKey(cfg.ID, parsed.BotID)
	a.mu.Lock()
	a.clients[key] = client
	a.mu.Unlock()

	connCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := client.Run(connCtx, AuthCredentials{
			BotID:      parsed.BotID,
			Credential: parsed.Credential,
		}, func(frameCtx context.Context, frame WSFrame) error {
			return a.handleFrame(frameCtx, cfg, frame, handler)
		})
		if err != nil && connCtx.Err() == nil {
			a.logger.ErrorContext(ctx, "wecom websocket stopped",
				slog.String("config_id", cfg.ID),
				slog.Any("error", err),
			)
		}
	}()

	stop := func(context.Context) error {
		cancel()
		_ = client.Close()
		<-done
		a.mu.Lock()
		if current, ok := a.clients[key]; ok && current == client {
			delete(a.clients, key)
		}
		a.mu.Unlock()
		return nil
	}
	return channel.NewConnection(cfg, stop), nil
}

func (a *WeComAdapter) Send(ctx context.Context, cfg channel.ChannelConfig, msg channel.PreparedOutboundMessage) error {
	logical := msg.LogicalMessage()
	targetKind, targetID, ok := parseTarget(logical.Target)
	if !ok {
		return errors.New("wecom target is required")
	}
	parsed, err := parseConfig(cfg.Credentials)
	if err != nil {
		return err
	}
	client := a.getClient(cfg.ID, parsed.BotID)
	if client == nil {
		return errors.New("wecom connection is not active")
	}
	if logical.Message.IsEmpty() {
		return errors.New("message is required")
	}
	var (
		payload  any
		cmd      string
		reqID    string
		buildErr error
	)
	if ctxMeta, ok := a.lookupCallbackContext(cfg.ID, msg.Message.Message.Reply); ok {
		payload, cmd, reqID, buildErr = buildPreparedRespondPayload(ctx, msg.Message, ctxMeta.ReqID)
	} else {
		_ = targetKind
		payload, cmd, reqID, buildErr = buildSendPayload(logical.Message, targetID)
	}
	if buildErr != nil {
		return buildErr
	}
	ack, err := client.Reply(ctx, reqID, cmd, payload)
	if err != nil {
		return err
	}
	if ack.ErrCode != 0 {
		return fmt.Errorf("wecom send failed: %s (code: %d)", strings.TrimSpace(ack.ErrMsg), ack.ErrCode)
	}
	return nil
}

func (a *WeComAdapter) sendRespondStream(ctx context.Context, cfg channel.ChannelConfig, msg channel.PreparedMessage, reqID string, streamID string, finish bool) error {
	parsed, err := parseConfig(cfg.Credentials)
	if err != nil {
		return err
	}
	client := a.getClient(cfg.ID, parsed.BotID)
	if client == nil {
		return errors.New("wecom connection is not active")
	}
	payload, cmd, ackReqID, err := buildPreparedRespondPayloadWithStream(ctx, msg, reqID, streamID, finish)
	if err != nil {
		return err
	}
	ack, err := client.Reply(ctx, ackReqID, cmd, payload)
	if err != nil {
		return err
	}
	if ack.ErrCode != 0 {
		return fmt.Errorf("wecom send failed: %s (code: %d)", strings.TrimSpace(ack.ErrMsg), ack.ErrCode)
	}
	return nil
}

func secondsOrDefault(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
