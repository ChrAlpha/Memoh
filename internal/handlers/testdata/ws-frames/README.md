# WebSocket frame goldens

Each file is one server-to-browser frame, written by the production send path in
`internal/handlers/ws_frames_golden_test.go`. The web client reads the same files in
`apps/web/src/store/chat/ws-frames.golden.test.ts`. The desktop app bundles
`@memohai/web` and connects to a hosted server, so it may be older than the server
that sends these frames.

- Adding a field is a compatible change: regenerate with
  `go test ./internal/handlers -run TestWSFramesGolden -update`.
- Removing a field, renaming it, or changing its type breaks desktop builds in the
  field. Confirm the minimum supported desktop version first, and keep sending the
  old shape until that version is out of support.
