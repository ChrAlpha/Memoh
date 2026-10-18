// Package arch mechanically enforces the channel-boundary dependency rules
// from docs/superpowers/specs/2026-07-17-channel-boundary-design.md §8.
// Every exemption below is deliberate and carries its rationale; removing
// code from an exemption list is always safe, adding to one is a design
// decision that belongs in the spec.
package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const modulePrefix = "github.com/felinics/memoh/"

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate arch test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// goFiles yields non-test .go files under dir (relative to root), with
// forward-slash paths relative to the repo root.
func goFiles(t *testing.T, root, dir string) []string {
	t.Helper()
	return collectGoFiles(t, root, dir, false)
}

// allGoFiles also includes tests. It is used where test fixtures must obey the
// same dependency boundary as production code rather than reaching into an
// implementation package for convenient setup.
func allGoFiles(t *testing.T, root, dir string) []string {
	t.Helper()
	return collectGoFiles(t, root, dir, true)
}

func collectGoFiles(t *testing.T, root, dir string, includeTests bool) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") ||
			(!includeTests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

func imports(t *testing.T, root, relPath string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, relPath), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", relPath, err)
	}
	var result []string
	for _, imp := range f.Imports {
		result = append(result, strings.Trim(imp.Path.Value, `"`))
	}
	return result
}

func isPackageOrChild(imp, packagePath string) bool {
	return imp == packagePath || strings.HasPrefix(imp, packagePath+"/")
}

// TestChannelAgentDependenciesStayOnPorts prevents Channel from reaching
// through the Agent facade into orchestration or runtime implementations.
func TestChannelAgentDependenciesStayOnPorts(t *testing.T) {
	allowedAgentRoots := []string{
		modulePrefix + "internal/agent/turn",
		modulePrefix + "internal/agent/event",
		modulePrefix + "internal/agent/decision",
	}
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/channel") {
		for _, imp := range imports(t, root, file) {
			if !isPackageOrChild(imp, modulePrefix+"internal/agent") {
				continue
			}
			allowed := false
			for _, packagePath := range allowedAgentRoots {
				if isPackageOrChild(imp, packagePath) {
					allowed = true
					break
				}
			}
			if !allowed {
				t.Errorf("%s imports %s: Channel may only depend on agent/turn, agent/event, or agent/decision", file, imp)
			}
		}
	}
}

// TestTimelineOnlyDependsOnTurnPort keeps the chat timeline independent of
// both Channel and Agent implementations. Timeline may translate a completed
// turn response, but it may not start or orchestrate one.
func TestTimelineOnlyDependsOnTurnPort(t *testing.T) {
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/chat/timeline") {
		for _, imp := range imports(t, root, file) {
			switch {
			case isPackageOrChild(imp, modulePrefix+"internal/channel"):
				t.Errorf("%s imports %s: chat/timeline must not depend on Channel", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/agent") &&
				!isPackageOrChild(imp, modulePrefix+"internal/agent/turn"):
				t.Errorf("%s imports %s: chat/timeline may only depend on the Agent turn contract", file, imp)
			}
		}
	}
}

// TestChatStorageDomainsDoNotDependOnUpperLayers keeps persisted thread and
// message state below both Agent execution and Channel delivery. Timeline has
// its separate, narrower turn-port allowance above.
func TestChatStorageDomainsDoNotDependOnUpperLayers(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"internal/chat/thread", "internal/chat/message"} {
		for _, file := range allGoFiles(t, root, dir) {
			for _, imp := range imports(t, root, file) {
				switch {
				case isPackageOrChild(imp, modulePrefix+"internal/agent"):
					t.Errorf("%s imports %s: chat storage domains must not depend on Agent", file, imp)
				case isPackageOrChild(imp, modulePrefix+"internal/channel"):
					t.Errorf("%s imports %s: chat storage domains must not depend on Channel", file, imp)
				case isPackageOrChild(imp, modulePrefix+"internal/handlers"):
					t.Errorf("%s imports %s: chat storage domains must not depend on HTTP handlers", file, imp)
				}
			}
		}
	}
}

// agentParent is the parent package internal/agent: the Service facade and the
// turn port. Its path is the prefix of every package under it, so a rule about
// the parent alone must compare the path exactly, never with isPackageOrChild.
const agentParent = "internal/agent"

// applicationLayerDirs are the packages that make up turn orchestration besides
// the parent package: the components split out of it. Rules that bind
// orchestration, or keep lower layers from reaching it, cover the parent
// (applicationLayerFiles, isApplicationLayer) and all of them.
var applicationLayerDirs = []string{
	"internal/agent/runreq",
	"internal/agent/outcome",
	"internal/agent/activeprompt",
	"internal/agent/botenv",
	"internal/agent/transcript",
	"internal/agent/lifecycle",
	"internal/agent/notify",
	"internal/agent/persist",
	"internal/agent/drive",
	"internal/agent/drivetest",
	"internal/agent/memory",
	"internal/agent/compact",
	"internal/agent/decide",
	"internal/agent/admit",
	"internal/agent/runconfig",
	"internal/agent/subagent",
	"internal/agent/externalrun",
	"internal/agent/plan",
	"internal/agent/pending",
	"internal/agent/answer",
	"internal/agent/resume",
	"internal/agent/schedule",
	"internal/agent/web",
	"internal/agent/title",
	"internal/agent/agenttest",
}

// applicationLayerFiles lists the Go files of the parent package itself and of
// every package in applicationLayerDirs. The parent's own files are only those
// directly in internal/agent.
func applicationLayerFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, agentParent))
	if err != nil {
		t.Fatalf("read %s: %v", agentParent, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			files = append(files, agentParent+"/"+entry.Name())
		}
	}
	for _, dir := range applicationLayerDirs {
		files = append(files, allGoFiles(t, root, dir)...)
	}
	return files
}

// isApplicationLayer reports whether imp is the parent package or one of
// applicationLayerDirs.
func isApplicationLayer(imp string) bool {
	if imp == modulePrefix+agentParent {
		return true
	}
	for _, dir := range applicationLayerDirs {
		if isPackageOrChild(imp, modulePrefix+dir) {
			return true
		}
	}
	return false
}

// TestApplicationDoesNotDependOnChannel keeps use-case orchestration on
// neutral Agent/Chat ports. Platform identity and conversation vocabulary
// must enter through adapters at the composition root.
func TestApplicationDoesNotDependOnChannel(t *testing.T) {
	root := repoRoot(t)
	for _, file := range applicationLayerFiles(t, root) {
		for _, imp := range imports(t, root, file) {
			if isPackageOrChild(imp, modulePrefix+"internal/channel") {
				t.Errorf("%s imports %s: Agent application must consume neutral ports, not Channel", file, imp)
			}
		}
	}
}

// TestAgentContextDoesNotDependOnExecutionOrDelivery keeps context assembly
// reusable by application and runtimes without creating a reverse dependency.
// Persistence-backed compaction is intentionally allowed to use db/sqlc until
// its store adapter is extracted.
func TestAgentContextDoesNotDependOnExecutionOrDelivery(t *testing.T) {
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/agent/context") {
		for _, imp := range imports(t, root, file) {
			switch {
			case isApplicationLayer(imp),
				isPackageOrChild(imp, modulePrefix+"internal/agent/runtime"),
				isPackageOrChild(imp, modulePrefix+"internal/agent/tool"),
				isPackageOrChild(imp, modulePrefix+"internal/agent/adapter"):
				t.Errorf("%s imports %s: agent/context must not depend on Agent execution layers", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/channel"):
				t.Errorf("%s imports %s: agent/context must not depend on Channel", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/handlers"):
				t.Errorf("%s imports %s: agent/context must not depend on HTTP handlers", file, imp)
			case strings.HasPrefix(imp, "github.com/labstack/echo"):
				t.Errorf("%s imports Echo", file)
			case strings.HasPrefix(imp, "go.uber.org/fx"):
				t.Errorf("%s imports fx", file)
			}
		}
	}
}

// TestNativeRuntimeStaysBelowApplicationAndDelivery keeps the in-process
// runtime reusable behind the application service. Native execution may use
// Agent ports and lower-level domains, but it must not reach back into turn
// orchestration or either delivery layer.
func TestNativeRuntimeStaysBelowApplicationAndDelivery(t *testing.T) {
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/agent/runtime/native") {
		for _, imp := range imports(t, root, file) {
			switch {
			case isApplicationLayer(imp):
				t.Errorf("%s imports %s: native runtime must not depend on Agent application orchestration", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/channel"):
				t.Errorf("%s imports %s: native runtime must not depend on Channel delivery", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/handlers"):
				t.Errorf("%s imports %s: native runtime must not depend on HTTP handlers", file, imp)
			}
		}
	}
}

// TestAgentToolStaysOnNeutralPorts prevents tool providers from reaching into
// orchestration or either delivery implementation.
func TestAgentToolStaysOnNeutralPorts(t *testing.T) {
	root := repoRoot(t)
	for _, file := range allGoFiles(t, root, "internal/agent/tool") {
		for _, imp := range imports(t, root, file) {
			switch {
			case isApplicationLayer(imp):
				t.Errorf("%s imports %s: Agent tools must not depend on application orchestration", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/channel"):
				t.Errorf("%s imports %s: Agent tools must use neutral messaging/contact ports", file, imp)
			case isPackageOrChild(imp, modulePrefix+"internal/handlers"):
				t.Errorf("%s imports %s: Agent tools must not depend on HTTP handlers", file, imp)
			}
		}
	}
}

// TestAgentRuntimeDoesNotDependOnChat keeps runtime snapshots and execution
// independent from Thread/Message/view implementations.
func TestAgentRuntimeDoesNotDependOnChat(t *testing.T) {
	root := repoRoot(t)
	for _, file := range allGoFiles(t, root, "internal/agent/runtime") {
		for _, imp := range imports(t, root, file) {
			if isPackageOrChild(imp, modulePrefix+"internal/chat") {
				t.Errorf("%s imports %s: Agent runtime must consume Agent-owned contracts", file, imp)
			}
		}
	}
}

func TestDirectExternalRuntimesDoNotDependOnACP(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"internal/agent/runtime/codex", "internal/agent/runtime/claudecode"} {
		for _, file := range goFiles(t, root, dir) {
			for _, imp := range imports(t, root, file) {
				if isPackageOrChild(imp, modulePrefix+"internal/agent/runtime/acp") {
					t.Errorf("%s imports %s: direct External Agent runtimes must not depend on ACP", file, imp)
				}
			}
		}
	}
}

// TestACPNativeDirectRuntimesUseOnlyTheACPClient covers built-in agents whose
// own wire protocol is ACP. They share the protocol client, but the pool and
// the profile registry serve user-supplied agents only: a built-in agent that
// reached them would be configured by agent id inside the generic runtime.
func TestACPNativeDirectRuntimesUseOnlyTheACPClient(t *testing.T) {
	root := repoRoot(t)
	client := modulePrefix + "internal/agent/runtime/acp/client"
	for _, file := range allGoFiles(t, root, "internal/agent/runtime/grok") {
		for _, imp := range imports(t, root, file) {
			if isPackageOrChild(imp, modulePrefix+"internal/agent/runtime/acp") && imp != client {
				t.Errorf("%s imports %s: an ACP-native direct runtime may use only the ACP client", file, imp)
			}
		}
	}
}

// TestThreadStorageDoesNotReachRouteDB pins the ownership split: Thread may
// carry an opaque route_id, while Channel owns active-thread selection and
// route metadata projection.
func TestThreadStorageDoesNotReachRouteDB(t *testing.T) {
	root := repoRoot(t)
	forbidden := []string{
		"bot_channel_routes",
		"GetActiveSessionForRoute",
		"SetRouteActiveSession",
	}
	for _, file := range allGoFiles(t, root, "internal/chat/thread") {
		data, err := os.ReadFile(filepath.Join(root, file)) //nolint:gosec // guard reads repository sources
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, token := range forbidden {
			if strings.Contains(string(data), token) {
				t.Errorf("%s contains %q: route DB ownership belongs to internal/channel/route", file, token)
			}
		}
	}
	queryFile := filepath.Join(root, "db/postgres/queries/sessions.sql")
	data, err := os.ReadFile(queryFile) //nolint:gosec // fixed repository path
	if err != nil {
		t.Fatalf("read sessions.sql: %v", err)
	}
	for _, token := range forbidden {
		if strings.Contains(string(data), token) {
			t.Errorf("db/postgres/queries/sessions.sql contains %q: Thread queries must not join or mutate Route DB", token)
		}
	}
}

// TestChannelDoesNotImportEcho: webhook endpoints are the only HTTP surface
// the channel package owns (spec §8 exemption).
func TestChannelDoesNotImportEcho(t *testing.T) {
	exempt := map[string]string{
		"internal/channel/webhook_handler.go":                 "channel-owned webhook HTTP endpoint",
		"internal/channel/adapters/feishu/webhook_handler.go": "platform webhook endpoint",
		"internal/channel/adapters/line/adapter.go":           "platform webhook endpoint",
		"internal/channel/adapters/wechatoa/inbound.go":       "platform webhook endpoint",
		"internal/channel/adapters/weixin/qr_handler.go":      "weixin QR login HTTP endpoint",
	}
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/channel") {
		if _, ok := exempt[file]; ok {
			continue
		}
		for _, imp := range imports(t, root, file) {
			if strings.HasPrefix(imp, "github.com/labstack/echo") {
				t.Errorf("%s imports Echo outside the webhook-endpoint exemptions", file)
			}
		}
	}
}

// TestChannelAndTimelineDoNotImportFx: assembly lives in cmd/** only.
func TestChannelAndTimelineDoNotImportFx(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"internal/channel", "internal/chat/timeline"} {
		for _, file := range goFiles(t, root, dir) {
			for _, imp := range imports(t, root, file) {
				if strings.HasPrefix(imp, "go.uber.org/fx") {
					t.Errorf("%s imports fx: assembly belongs to cmd/**", file)
				}
			}
		}
	}
}

// TestTurnPortStaysPure: the contract package (and its transports) must not
// depend on HTTP frameworks, assembly, generated SQL, the channel side, or
// the application implementation.
func TestTurnPortStaysPure(t *testing.T) {
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/agent/turn") {
		for _, imp := range imports(t, root, file) {
			switch {
			case strings.HasPrefix(imp, "github.com/labstack/echo"):
				t.Errorf("%s imports Echo", file)
			case strings.HasPrefix(imp, "go.uber.org/fx"):
				t.Errorf("%s imports fx", file)
			case imp == modulePrefix+"internal/db/postgres/sqlc":
				t.Errorf("%s imports generated sqlc", file)
			case imp == modulePrefix+"internal/channel" || strings.HasPrefix(imp, modulePrefix+"internal/channel/"):
				t.Errorf("%s imports the channel side", file)
			case turnForbiddenLayerImport(imp):
				t.Errorf("%s imports %s: the turn contract must not depend on retired domains, application, or runtime", file, imp)
			}
		}
	}
}

func turnForbiddenLayerImport(imp string) bool {
	for _, packagePath := range []string{
		modulePrefix + "internal/conversation",
		modulePrefix + "internal/session",
		modulePrefix + "internal/message",
		modulePrefix + "internal/pipeline",
		modulePrefix + "internal/toolapproval",
		modulePrefix + "internal/userinput",
		modulePrefix + "internal/agent/runtime",
	} {
		if isPackageOrChild(imp, packagePath) {
			return true
		}
	}
	return isApplicationLayer(imp)
}

// TestRetiredDomainPackagesStayRemoved prevents compatibility imports from
// quietly recreating the pre-split package boundaries.
func TestRetiredDomainPackagesStayRemoved(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{
		"internal/acpagent",
		"internal/acpclient",
		"internal/acpfeedback",
		"internal/agentfeedback",
		"internal/agent/decision/feedback",
		"internal/acpprofile",
		"internal/agentpayload",
		"internal/conversation",
		"internal/decision",
		"internal/session",
		"internal/message",
		"internal/pipeline",
		"internal/toolapproval",
		"internal/userinput",
		"internal/contextfrag",
		"internal/historyfrag",
		"internal/compaction",
		"internal/contextlimit",
		"internal/sessionruntime",
		"internal/agent/tools",
	} {
		var files []string
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk retired package %s: %v", dir, err)
		}
		if len(files) != 0 {
			t.Errorf("%s contains Go files after its responsibilities moved to agent/* or chat/*", dir)
		}
	}
}

// agentLayers is the order of the packages split out of the agent application.
// A package imports only packages of a lower layer; packages of one layer do not
// import each other, and none imports the parent package internal/agent.
var agentLayers = map[string]int{
	"runreq":       0,
	"outcome":      1,
	"activeprompt": 1,
	"botenv":       1,
	"transcript":   2,
	"lifecycle":    2,
	"persist":      2,
	"memory":       3,
	"compact":      3,
	"decide":       3,
	"admit":        3,
	"runconfig":    4,
	"drive":        4,
	"title":        5,
	"notify":       5,
	"externalrun":  6,
	"plan":         7,
	"pending":      8,
	"web":          9,
	"schedule":     9,
	"resume":       9,
	"answer":       9,
	"subagent":     9,
}

// agentSubpackage names the agentLayers package imp belongs to, if any.
func agentSubpackage(imp string) (string, bool) {
	rest, ok := strings.CutPrefix(imp, modulePrefix+agentParent+"/")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	_, layered := agentLayers[name]
	return name, layered
}

// TestAgentSubpackagesImportDownward keeps the components of the agent in one
// direction: the facade at internal/agent assembles them, and they never reach
// back into it or sideways into a package of their own layer.
func TestAgentSubpackagesImportDownward(t *testing.T) {
	root := repoRoot(t)
	for name, layer := range agentLayers {
		for _, file := range allGoFiles(t, root, agentParent+"/"+name) {
			for _, imp := range imports(t, root, file) {
				if imp == modulePrefix+agentParent {
					t.Errorf("%s imports %s: a component must not import the agent facade", file, imp)
					continue
				}
				other, layered := agentSubpackage(imp)
				if layered && other != name && agentLayers[other] >= layer {
					t.Errorf("%s imports %s: %s (layer %d) may import only packages of a lower layer, %s is layer %d",
						file, imp, name, layer, other, agentLayers[other])
				}
			}
		}
	}
}

// TestAgentEntryPackagesDoNotImportEachOther keeps the packages that start a
// run apart: each is an entry into the same lower components, and an entry that
// called another would start a run through a path the lower layers do not know.
func TestAgentEntryPackagesDoNotImportEachOther(t *testing.T) {
	entries := []string{"web", "schedule", "resume", "answer", "subagent"}
	root := repoRoot(t)
	for _, name := range entries {
		for _, file := range allGoFiles(t, root, agentParent+"/"+name) {
			for _, imp := range imports(t, root, file) {
				other, layered := agentSubpackage(imp)
				if layered && other != name && slices.Contains(entries, other) {
					t.Errorf("%s imports %s: entry packages must not import each other", file, imp)
				}
			}
		}
	}
}

// TestRetiredAgentApplicationStaysRemoved keeps the directory the orchestration
// used to live in from coming back.
func TestRetiredAgentApplicationStaysRemoved(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, agentParent, "application")); !os.IsNotExist(err) {
		t.Errorf("%s/application exists: the orchestration is the package %s itself", agentParent, agentParent)
	}
}

// TestDefaultTeamIDReferences: business packages must not hardcode the
// single-team assumption. Each exemption is a deliberate singleton-team
// touchpoint that a hosted multi-team runtime replaces wholesale.
func TestDefaultTeamIDReferences(t *testing.T) {
	allowedPrefixes := []string{
		"cmd/",
		"internal/db/",
		"internal/team/",
	}
	exempt := map[string]string{
		"internal/memory/adapters/registry.go":               "memory provider registry resolves the singleton team when no scope is bound",
		"internal/memory/adapters/builtin/pgvector_index.go": "builtin pgvector index binds the singleton team resolver",
		"internal/channel/service.go":                        "configless channels (web/cli) synthesize a config; turn.Service fails closed on empty TeamID",
	}
	ref := regexp.MustCompile(`\bteam\.DefaultTeamID\b`)
	root := repoRoot(t)
	for _, dir := range []string{"internal", "cmd"} {
		for _, file := range goFiles(t, root, dir) {
			data, err := os.ReadFile(filepath.Join(root, file)) //nolint:gosec // guard test walks repo-tracked files by design
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			if !ref.Match(data) {
				continue
			}
			if _, ok := exempt[file]; ok {
				continue
			}
			allowed := false
			for _, prefix := range allowedPrefixes {
				if strings.HasPrefix(file, prefix) {
					allowed = true
					break
				}
			}
			if !allowed {
				t.Errorf("%s references team.DefaultTeamID outside the allowed layers (internal/db, cmd/**, tests) without a documented exemption", file)
			}
		}
	}
}

// TestStandaloneChannelDoesNotOwnStorageBackends keeps the split Channel
// process independent from Server's workspace and media filesystem. Channel
// may use the storage port through internal RPC, but it must not assemble a
// concrete local/container storage provider of its own.
func TestStandaloneChannelDoesNotOwnStorageBackends(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"cmd/channel", "cmd/internal/channel"} {
		for _, file := range goFiles(t, root, dir) {
			for _, imp := range imports(t, root, file) {
				if isPackageOrChild(imp, modulePrefix+"internal/storage/providers") {
					t.Errorf("%s imports %s: standalone Channel storage must go through the Server RPC", file, imp)
				}
			}
		}
	}
}

// TestRouteCredentialsHaveOneIssuer keeps the identities a channel turn
// carries signed in one place. The inbound path and the discuss admission
// gather their inputs from their own reads and sign through
// auth.IssueRouteCredentials; a second signing site would let a turn started by
// one of them name someone else than the same turn started by the other.
func TestRouteCredentialsHaveOneIssuer(t *testing.T) {
	root := repoRoot(t)
	forbidden := regexp.MustCompile(`\bauth\.(GenerateChatToken|GenerateToken)\b`)
	files := goFiles(t, root, "internal/channel")
	for _, file := range applicationLayerFiles(t, root) {
		if !strings.HasSuffix(file, "_test.go") {
			files = append(files, file)
		}
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(root, file)) //nolint:gosec // guard test walks repo-tracked files by design
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if found := forbidden.Find(data); found != nil {
			t.Errorf("%s calls %s: sign a turn's credentials through auth.IssueRouteCredentials", file, found)
		}
	}
}

// TestRunTerminalStateHasOneWriter keeps the write of a run's terminal state
// on the session runtime in drive/finish.go. drive.Driver.Finish composes it
// with the lifecycle row and the outcome record, so a second write site would
// end a run without them. The session runtime package owns the methods; the
// agenttest doubles implement them for tests.
//
// The rule is by method name alone: nothing but the session runtime and
// drive/finish.go calls FinishRun, FinishRunWithErrorCode or
// FinishRunWithFailure, and the driver's own entry is named Finish.
func TestRunTerminalStateHasOneWriter(t *testing.T) {
	root := repoRoot(t)
	for _, file := range goFiles(t, root, "internal/agent") {
		if file == "internal/agent/drive/finish.go" ||
			strings.HasPrefix(file, "internal/agent/runtime/session/") ||
			strings.HasPrefix(file, "internal/agent/agenttest/") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "FinishRun", "FinishRunWithErrorCode", "FinishRunWithFailure":
				t.Errorf("%s calls %s: a run's terminal state is written in internal/agent/drive/finish.go only; end a run through drive.Driver.Finish", file, sel.Sel.Name)
			}
			return true
		})
	}
}
