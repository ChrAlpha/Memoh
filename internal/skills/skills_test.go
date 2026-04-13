package skills

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
)

func TestParseFileFallbacks(t *testing.T) {
	raw := "# Use this skill\n\nDo something useful."
	got := ParseFile(raw, "plain-skill")

	if got.Name != "plain-skill" {
		t.Fatalf("expected name plain-skill, got %q", got.Name)
	}
	if got.Description != "plain-skill" {
		t.Fatalf("expected description plain-skill, got %q", got.Description)
	}
	if got.Content != raw {
		t.Fatalf("expected content to keep original markdown, got %q", got.Content)
	}
}

func TestResolveSupportsDisabledFallbackAndShadowing(t *testing.T) {
	items := []Entry{
		{Name: "alpha", SourcePath: "/data/skills/alpha/SKILL.md", Managed: true, SourceKind: SourceKindManaged},
		{Name: "alpha", SourcePath: "/data/.openclaw/skills/alpha/SKILL.md", SourceKind: SourceKindCompat},
		{Name: "beta", SourcePath: "/data/.openclaw/skills/beta/SKILL.md", SourceKind: SourceKindCompat},
	}

	resolved := resolve(items, map[string]indexOverride{
		"/data/skills/alpha/SKILL.md": {Disabled: true},
	})

	managedAlpha, ok := findBySourcePath(resolved, "/data/skills/alpha/SKILL.md")
	if !ok {
		t.Fatalf("managed alpha not found in resolved items")
	}
	if managedAlpha.State != StateDisabled {
		t.Fatalf("managed alpha state = %q, want disabled", managedAlpha.State)
	}
	compatAlpha, ok := findBySourcePath(resolved, "/data/.openclaw/skills/alpha/SKILL.md")
	if !ok {
		t.Fatalf("compat alpha not found in resolved items")
	}
	if compatAlpha.State != StateEffective {
		t.Fatalf("compat alpha state = %q, want effective", compatAlpha.State)
	}
	beta, ok := findBySourcePath(resolved, "/data/.openclaw/skills/beta/SKILL.md")
	if !ok {
		t.Fatalf("beta not found in resolved items")
	}
	if beta.State != StateEffective {
		t.Fatalf("beta state = %q, want effective", beta.State)
	}
}

func TestListReadsFullRawContentAndWritesIndex(t *testing.T) {
	client := newFakeClient()
	client.listings[ManagedDirPath] = []*pb.FileEntry{{Path: "alpha", IsDir: true}}
	client.files[pathJoin(ManagedDirPath, "alpha", "SKILL.md")] = "---\nname: alpha\ndescription: Alpha\n---\n\n" + strings.Repeat("A", 7000)

	items, err := List(context.Background(), client)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if len(items[0].Raw) <= 7000 {
		t.Fatalf("expected full raw content, got len=%d", len(items[0].Raw))
	}
	if _, ok := client.files[IndexFilePath]; !ok {
		t.Fatalf("expected index file to be written")
	}
}

func TestApplyActionAdoptAndDisable(t *testing.T) {
	client := newFakeClient()
	externalPath := pathJoin("/data/.openclaw/skills", "alpha", "SKILL.md")
	client.listings["/data/.openclaw/skills"] = []*pb.FileEntry{{Path: "alpha", IsDir: true}}
	client.files[externalPath] = "---\nname: alpha\ndescription: Alpha\n---\n\n# Alpha"

	if err := ApplyAction(context.Background(), client, ActionRequest{
		Action:     ActionAdopt,
		TargetPath: externalPath,
	}); err != nil {
		t.Fatalf("adopt returned error: %v", err)
	}
	if _, ok := client.files[pathJoin(ManagedDirPath, "alpha", "SKILL.md")]; !ok {
		t.Fatalf("expected managed copy after adopt")
	}

	if err := ApplyAction(context.Background(), client, ActionRequest{
		Action:     ActionDisable,
		TargetPath: externalPath,
	}); err != nil {
		t.Fatalf("disable returned error: %v", err)
	}
	idx := readIndex(context.Background(), client)
	if !idx.Overrides[externalPath].Disabled {
		t.Fatalf("expected disabled override for %s", externalPath)
	}
}

func TestDiscoveryRootsStartWithManagedAndLegacy(t *testing.T) {
	roots := DiscoveryRoots()
	if len(roots) < 2 {
		t.Fatalf("expected at least 2 discovery roots, got %d", len(roots))
	}
	if roots[0].Path != ManagedDirPath || !roots[0].Managed {
		t.Fatalf("first root = %+v, want managed %q", roots[0], ManagedDirPath)
	}
	if roots[1].Path != LegacyDirPath || roots[1].Managed {
		t.Fatalf("second root = %+v, want legacy %q", roots[1], LegacyDirPath)
	}
}

func TestContainerEnvUsesDataHomeAndXDGDirs(t *testing.T) {
	env := ContainerEnv()
	for _, want := range []string{
		"HOME=/data",
		"XDG_CONFIG_HOME=/data/.config",
		"XDG_DATA_HOME=/data/.local/share",
		"XDG_CACHE_HOME=/data/.cache",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("env %+v does not contain %q", env, want)
		}
	}
}

type fakeClient struct {
	listings map[string][]*pb.FileEntry
	files    map[string]string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		listings: make(map[string][]*pb.FileEntry),
		files:    make(map[string]string),
	}
}

func (f *fakeClient) ListDirAll(_ context.Context, p string, _ bool) ([]*pb.FileEntry, error) {
	items, ok := f.listings[p]
	if !ok {
		return nil, io.EOF
	}
	return items, nil
}

func (f *fakeClient) ReadRaw(_ context.Context, p string) (io.ReadCloser, error) {
	content, ok := f.files[p]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *fakeClient) WriteRaw(_ context.Context, p string, r io.Reader) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.files[p] = string(data)
	return int64(len(data)), nil
}

func (*fakeClient) Mkdir(_ context.Context, _ string) error {
	return nil
}

func pathJoin(parts ...string) string {
	return strings.Join(parts, "/")
}
