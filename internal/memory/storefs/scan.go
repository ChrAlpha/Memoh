package storefs

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	pb "github.com/felinics/memoh/internal/workspace/bridgepb"
)

// workspaceFiles is the workspace file access the store uses; *bridge.Client
// provides it.
type workspaceFiles interface {
	ListDirAll(ctx context.Context, path string, recursive bool) ([]*pb.FileEntry, error)
	ReadRaw(ctx context.Context, path string) (io.ReadCloser, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	DeleteFile(ctx context.Context, path string, recursive bool) error
}

// scannedFile is a memory markdown file as last read from the workspace.
type scannedFile struct {
	// listedPath is the path as the workspace listed it; path is its
	// absolute form under the data mount.
	listedPath string
	path       string
	// stamp is the size and modification time the file had when read.
	stamp  string
	digest [sha256.Size]byte
	// items holds the parsed entries; parsed is false for a file that is
	// not memory markdown (e.g. free-form notes the agent wrote).
	items  []MemoryItem
	parsed bool
}

// fileScans caches each bot's parsed memory files. A scan lists the memory
// directory once and reuses an entry while the listing reports the same size
// and modification time, so it reads only the files that changed since the
// last scan, whoever changed them. The stamp has one-second resolution: a
// rewrite that keeps the size within the second it was read goes unseen until
// the file changes again. Writes through the Service drop the entries they
// touch, so the Service's own writes never hit that window.
type fileScans struct {
	mu   sync.Mutex
	bots map[string]*botScan
}

type botScan struct {
	files map[string]scannedFile
	// generation counts writes, so a scan that listed the directory before a
	// write does not install entries the write made stale.
	generation uint64
}

func (c *fileScans) bot(botID string) *botScan {
	if c.bots == nil {
		c.bots = make(map[string]*botScan)
	}
	b := c.bots[botID]
	if b == nil {
		b = &botScan{}
		c.bots[botID] = b
	}
	return b
}

func (c *fileScans) snapshot(botID string) (map[string]scannedFile, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bot(botID)
	return b.files, b.generation
}

func (c *fileScans) install(botID string, generation uint64, files map[string]scannedFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bot(botID)
	if b.generation == generation {
		b.files = files
	}
}

// forget drops the entries for paths written or deleted under a bot; with no
// paths it drops all of the bot's entries.
func (c *fileScans) forget(botID string, paths ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bot(botID)
	b.generation++
	if len(paths) == 0 {
		b.files = nil
		return
	}
	if b.files == nil {
		return
	}
	next := make(map[string]scannedFile, len(b.files))
	for path, f := range b.files {
		next[path] = f
	}
	for _, path := range paths {
		delete(next, path)
	}
	b.files = next
}

func fileStamp(entry *pb.FileEntry) string {
	if entry.GetModTime() == "" {
		return ""
	}
	return strconv.FormatInt(entry.GetSize(), 10) + "@" + entry.GetModTime()
}

// scanMemoryFiles returns the bot's memory markdown files in listing order,
// reading only those changed since the previous scan. A file that cannot be
// read is left out.
func (s *Service) scanMemoryFiles(ctx context.Context, botID string) ([]scannedFile, error) {
	files, err := s.workspace(ctx, botID)
	if err != nil {
		return nil, err
	}
	entries, err := files.ListDirAll(ctx, memoryDirPath(), true)
	if err != nil {
		if isNotFound(err) {
			s.scans.forget(botID)
			return nil, nil
		}
		return nil, err
	}
	previous, generation := s.scans.snapshot(botID)
	next := make(map[string]scannedFile, len(entries))
	out := make([]scannedFile, 0, len(entries))
	for _, entry := range entries {
		if entry.GetIsDir() || !strings.HasSuffix(entry.GetPath(), ".md") {
			continue
		}
		path := memoryEntryPath(entry.GetPath())
		stamp := fileStamp(entry)
		if f, ok := previous[path]; ok && stamp != "" && f.stamp == stamp {
			f.listedPath = entry.GetPath()
			next[path] = f
			out = append(out, f)
			continue
		}
		content, err := readWorkspaceFile(ctx, files, path)
		if err != nil {
			s.logger.WarnContext(ctx, "memory scan: failed to read memory file",
				slog.String("bot_id", botID), slog.String("path", path), slog.Any("error", err))
			continue
		}
		f := scannedFile{listedPath: entry.GetPath(), path: path, stamp: stamp, digest: sha256.Sum256([]byte(content))}
		if items, err := parseMemoryFile(content); err == nil {
			f.items = items
			f.parsed = true
		}
		next[path] = f
		out = append(out, f)
	}
	s.scans.install(botID, generation, next)
	return out, nil
}

func readWorkspaceFile(ctx context.Context, files workspaceFiles, path string) (string, error) {
	reader, err := files.ReadRaw(ctx, path)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// cloneMemoryItem copies item and its metadata map, so callers can change what
// they read without touching the scan cache.
func cloneMemoryItem(item MemoryItem) MemoryItem {
	if item.Metadata != nil {
		meta := make(map[string]any, len(item.Metadata))
		for k, v := range item.Metadata {
			meta[k] = v
		}
		item.Metadata = meta
	}
	return item
}
