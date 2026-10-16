package wechatoa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/felinics/memoh/internal/channel"
)

const Type channel.ChannelType = "wechatoa"

const wechatoaVerificationTimeout = 15 * time.Second

// customerMessageWindow is how long after a user's message the customer
// service message API accepts replies to that user ("用户发送消息: 5条, 48小时",
// 发送客服消息, developers.weixin.qq.com/doc/service/api/customer/message/api_sendcustommessage.html).
const customerMessageWindow = 48 * time.Hour

type WeChatOAAdapter struct {
	logger *slog.Logger

	mu      sync.RWMutex
	clients map[string]*apiClient
}

func NewWeChatOAAdapter(log *slog.Logger) *WeChatOAAdapter {
	if log == nil {
		log = slog.Default()
	}
	return &WeChatOAAdapter{
		logger:  log.With(slog.String("adapter", "wechatoa")),
		clients: make(map[string]*apiClient),
	}
}

func (*WeChatOAAdapter) Type() channel.ChannelType { return Type }

func (*WeChatOAAdapter) Descriptor() channel.Descriptor {
	return channel.Descriptor{
		Type:        Type,
		DisplayName: "WeChat Official Account",
		Capabilities: channel.ChannelCapabilities{
			Text:           true,
			Attachments:    true,
			Media:          true,
			Reply:          true,
			BlockStreaming: true,
			ChatTypes:      []string{channel.ConversationTypePrivate},
			// The reply goes out as customer service messages when the run ends.
			// The API only reaches users who wrote to the account in the last 48
			// hours; there is no proactive alternative.
			Preview:     channel.PreviewNone,
			ReplyWindow: customerMessageWindow,
		},
		ConfigSchema: channel.ConfigSchema{
			Version: 1,
			Fields: map[string]channel.FieldSchema{
				"appId":          {Type: channel.FieldString, Required: true, Order: 0, Title: "App ID"},
				"appSecret":      {Type: channel.FieldSecret, Required: true, Order: 10, Title: "App Secret"},
				"token":          {Type: channel.FieldSecret, Required: true, Order: 20, Title: "Token"},
				"encodingAESKey": {Type: channel.FieldSecret, Order: 30, Title: "Encoding AES Key"},
				"httpProxyUrl": {
					Type:        channel.FieldSecret,
					Order:       40,
					Title:       "HTTP Proxy URL",
					Description: "Optional outbound HTTP proxy URL for WeChat API requests, e.g. http://user:pass@host:port. Explicit adapter proxy overrides HTTP_PROXY/HTTPS_PROXY.",
				},
				"encryptionMode": {
					Type:     channel.FieldEnum,
					Order:    50,
					Title:    "Encryption Mode",
					Required: true,
					Example:  encryptionModeSafe,
					Enum: []string{
						encryptionModeSafe,
						encryptionModeCompat,
						encryptionModePlain,
					},
				},
			},
		},
		UserConfigSchema: channel.ConfigSchema{
			Version: 1,
			Fields: map[string]channel.FieldSchema{
				"openid":  {Type: channel.FieldString},
				"unionid": {Type: channel.FieldString},
			},
		},
		TargetSpec: channel.TargetSpec{
			Format: "openid:xxx",
			Hints: []channel.TargetHint{
				{Label: "OpenID", Example: "openid:o6_bmjrPTlm6_2sgVt7hMZOPfL2M"},
			},
		},
		OutboundPolicy: channel.OutboundPolicy{
			TextChunkLimit: 600,
		},
	}
}

func (*WeChatOAAdapter) SelfIdentityPolicy() channel.SelfIdentityPolicy {
	return channel.SelfIdentityPolicy{
		RefreshOnCredentialsChange: true,
		RequireDiscoveryOnEnable:   true,
		RequiredSelfIdentityKey:    "app_id",
		DiscoveryErrorMessage:      "wechat official account credential verification failed",
		MissingIdentityMessage:     "wechat official account verification returned no app id",
	}
}

func (*WeChatOAAdapter) NormalizeConfig(raw map[string]any) (map[string]any, error) {
	return normalizeConfig(raw)
}

func (*WeChatOAAdapter) NormalizeUserConfig(raw map[string]any) (map[string]any, error) {
	return normalizeUserConfig(raw)
}

func (*WeChatOAAdapter) NormalizeTarget(raw string) string {
	return normalizeTarget(raw)
}

func (*WeChatOAAdapter) ResolveTarget(userConfig map[string]any) (string, error) {
	return resolveTarget(userConfig)
}

func (*WeChatOAAdapter) MatchBinding(config map[string]any, criteria channel.BindingCriteria) bool {
	return matchBinding(config, criteria)
}

func (*WeChatOAAdapter) BuildUserConfig(identity channel.Identity) map[string]any {
	return buildUserConfig(identity)
}

func (a *WeChatOAAdapter) DiscoverSelf(ctx context.Context, credentials map[string]any) (map[string]any, string, error) {
	cfg, err := parseConfig(credentials)
	if err != nil {
		return nil, "", err
	}
	if _, err := newSecurityVerifier(cfg.Token, cfg.EncodingAESKey, cfg.AppID); err != nil {
		return nil, "", fmt.Errorf("wechatoa validate webhook security: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, wechatoaVerificationTimeout)
	defer cancel()
	client, err := a.clientForConfig(credentials)
	if err != nil {
		return nil, "", err
	}
	if _, err := client.getAccessToken(callCtx); err != nil {
		return nil, "", fmt.Errorf("wechatoa discover self: %w", err)
	}
	id := strings.TrimSpace(cfg.AppID)
	return map[string]any{"app_id": id}, id, nil
}

func (*WeChatOAAdapter) Connect(ctx context.Context, cfg channel.ChannelConfig, handler channel.InboundHandler) (channel.Connection, error) {
	_ = ctx
	_ = handler
	if _, err := parseConfig(cfg.Credentials); err != nil {
		return nil, err
	}
	return channel.NewConnection(cfg, func(context.Context) error { return nil }), nil
}

func (a *WeChatOAAdapter) HandleWebhook(ctx context.Context, cfg channel.ChannelConfig, handler channel.InboundHandler, r *http.Request, w http.ResponseWriter) error {
	parsed, err := parseConfig(cfg.Credentials)
	if err != nil {
		return err
	}
	verifier, err := newSecurityVerifier(parsed.Token, parsed.EncodingAESKey, parsed.AppID)
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		return handleVerifyRequest(verifier, parsed.EncryptionMode, r, w)
	case http.MethodPost:
		return a.handleInbound(ctx, verifier, parsed.EncryptionMode, cfg, handler, r, w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil
	}
}

func (a *WeChatOAAdapter) Send(ctx context.Context, cfg channel.ChannelConfig, msg channel.PreparedOutboundMessage) error {
	openID, ok := parseTarget(msg.Target)
	if !ok {
		return errors.New("wechatoa target is required")
	}
	client, err := a.clientForConfig(cfg.Credentials)
	if err != nil {
		return err
	}
	if a.logger != nil {
		a.logger.DebugContext(ctx, "wechatoa send start",
			slog.String("config_id", cfg.ID),
			slog.String("target", openID),
			slog.Bool("has_text", strings.TrimSpace(msg.Message.Message.PlainText()) != ""),
			slog.Int("attachments", len(msg.Message.Attachments)),
		)
	}
	if err := client.sendPreparedMessage(ctx, openID, msg.Message); err != nil {
		return err
	}
	if a.logger != nil {
		a.logger.InfoContext(ctx, "wechatoa send success",
			slog.String("config_id", cfg.ID),
			slog.String("target", openID),
		)
	}
	return nil
}

func (a *WeChatOAAdapter) ResolveAttachment(ctx context.Context, cfg channel.ChannelConfig, attachment channel.Attachment) (channel.AttachmentPayload, error) {
	client, err := a.clientForConfig(cfg.Credentials)
	if err != nil {
		return channel.AttachmentPayload{}, err
	}
	mediaID := strings.TrimSpace(attachment.PlatformKey)
	if mediaID == "" {
		return channel.AttachmentPayload{}, errors.New("wechatoa platform_key(media_id) is required")
	}
	return client.downloadMedia(ctx, mediaID)
}
