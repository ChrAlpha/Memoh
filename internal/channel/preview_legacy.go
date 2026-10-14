package channel

import (
	"context"
	"errors"
)

// openLegacyStream opens the Push/Close session of an adapter that declares
// PreviewLegacy, on a resolved target.
//
// This file is the legacy branch of OpenPreview. It goes away when the last
// adapter implements PreviewOpener.
func (s *managerSurface) openLegacyStream(ctx context.Context, target string, opts StreamOptions) (*managerOutboundStream, error) {
	if s.streamSender == nil {
		return nil, errors.New("channel stream sender not configured")
	}
	session, err := s.streamSender.OpenStream(ctx, s.config, target, opts)
	if err != nil {
		return nil, err
	}
	return s.newOutboundStream(target, opts, session, func(ctx context.Context) (PreparedOutboundStream, error) {
		return s.streamSender.OpenStream(ctx, s.config, target, continuation(opts))
	}), nil
}
