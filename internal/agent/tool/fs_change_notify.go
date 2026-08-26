package tools

import (
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"
)

// FSChangeNotifier receives the workspace paths touched by a successful
// fs-mutating tool execution. nil means unknown scope (exec, apply_patch, or
// a path that cannot be resolved host-side).
type FSChangeNotifier func(paths []string)

// WrapFSChangeNotify wraps the fs-mutating native tools so every successful
// execution reports its touched paths, regardless of which surface (web,
// channel, schedule, background) ran the turn. write/edit report their input
// path when absolute; exec and apply_patch report a wildcard because their
// impact is unknown without the agent's cwd or patch parsing.
func WrapFSChangeNotify(sdkTools []sdk.Tool, notify FSChangeNotifier) []sdk.Tool {
	if notify == nil || len(sdkTools) == 0 {
		return sdkTools
	}
	pathScoped := map[string]bool{
		ToolWrite().String(): true,
		ToolEdit().String():  true,
	}
	wildcard := map[string]bool{
		ToolExec().String():       true,
		ToolApplyPatch().String(): true,
	}
	wrapped := make([]sdk.Tool, len(sdkTools))
	copy(wrapped, sdkTools)
	for i := range wrapped {
		execute := wrapped[i].Execute
		if execute == nil {
			continue
		}
		name := strings.TrimSpace(wrapped[i].Name)
		isPathScoped := pathScoped[name]
		if !isPathScoped && !wildcard[name] {
			continue
		}
		wrapped[i].Execute = func(ctx *sdk.ToolExecContext, input any) (any, error) {
			output, err := execute(ctx, input)
			if err != nil {
				return output, err
			}
			if isPathScoped {
				notify(fsToolInputPaths(input))
			} else {
				notify(nil)
			}
			return output, nil
		}
	}
	return wrapped
}

func fsToolInputPaths(input any) []string {
	m, ok := input.(map[string]any)
	if !ok {
		return nil
	}
	path, ok := m["path"].(string)
	if !ok {
		return nil
	}
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		return nil
	}
	return []string{path}
}
