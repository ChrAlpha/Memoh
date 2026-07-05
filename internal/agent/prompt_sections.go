package agent

import (
	"slices"
	"strings"

	"github.com/memohai/memoh/internal/agent/sessionmode"
	"github.com/memohai/memoh/internal/contextfrag"
)

// SystemSection is one typed, priority-ordered piece of the system prompt.
type SystemSection struct {
	ID       string
	Kind     contextfrag.Kind
	Priority int
	Text     string
}

const (
	sectionIDIntro                 = "system.prompt.intro"
	sectionIDBotIdentity           = "system.bot_identity"
	sectionIDBody                  = "system.prompt.body"
	sectionIDTail                  = "system.prompt.tail"
	sectionIDPlatformIdentity      = "system.platform_identity"
	sectionIDSkills                = "system.skills"
	sectionIDWorkspaceInstructions = "system.workspace_instructions"
)

const (
	priorityIntro                 = 10
	priorityBotIdentity           = 20
	priorityBody                  = 30
	priorityTail                  = 50
	priorityPlatformIdentity      = 60
	prioritySkills                = 65
	priorityWorkspaceInstructions = 70
)

const (
	botInfoPlaceholder = "{{botInfoSection}}"
	workspaceHeading   = "## Workspace instruction files"
)

// GenerateSystemSections builds the typed system prompt sections that
// renderSystemSections joins into the same string GenerateSystemPrompt used
// to build directly. Two sections (intro, bot identity) sit inside a single
// template's own placeholder gaps: they are always returned, even with empty
// Text, so the uniform join below reproduces that template's literal
// spacing. Sections folding in dynamic, possibly-absent content (platform
// identity, skills, workspace files) are omitted entirely when empty.
func GenerateSystemSections(params SystemPromptParams) []SystemSection {
	home := "/data"
	timezone := strings.TrimSpace(params.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}

	intro, body, tailBoilerplate := splitSystemCommonTmpl(systemCommonTmpl)
	isSubagent := params.SessionType == sessionmode.Subagent

	sections := []SystemSection{
		{
			ID: sectionIDIntro, Kind: contextfrag.KindSystemPrompt, Priority: priorityIntro,
			Text: render(intro, map[string]string{"home": home, "timezone": timezone}),
		},
		{
			ID: sectionIDBotIdentity, Kind: contextfrag.KindBotIdentity, Priority: priorityBotIdentity,
			Text: buildBotInfoSection(params.Bot),
		},
		{ID: sectionIDBody, Kind: contextfrag.KindSystemPrompt, Priority: priorityBody, Text: body},
		{
			ID: sectionIDTail, Kind: contextfrag.KindSystemPrompt, Priority: priorityTail,
			Text: buildSystemPromptTail(tailBoilerplate, params.SessionType, isSubagent),
		},
	}

	if text := strings.TrimSpace(params.PlatformIdentitiesSection); text != "" {
		sections = append(sections, SystemSection{
			ID: sectionIDPlatformIdentity, Kind: contextfrag.KindPlatformIdentity, Priority: priorityPlatformIdentity, Text: text,
		})
	}

	if isSubagent {
		return sections
	}

	if text := strings.TrimSpace(buildSkillsSection(params.Skills)); text != "" {
		sections = append(sections, SystemSection{
			ID: sectionIDSkills, Kind: contextfrag.KindSystemPrompt, Priority: prioritySkills, Text: text,
		})
	}
	if text := strings.TrimSpace(buildFileSections(params.Files, params.MaxFilesBytes)); text != "" {
		sections = append(sections, SystemSection{
			ID: sectionIDWorkspaceInstructions, Kind: contextfrag.KindWorkspaceInstruction, Priority: priorityWorkspaceInstructions, Text: text,
		})
	}

	return sections
}

// renderSystemSections sorts sections by Priority ascending and joins them
// with a blank line between every consecutive pair. Emptiness is already
// resolved at construction time (which section a piece is, and whether it
// was appended at all), so the join never special-cases an empty Text.
func renderSystemSections(sections []SystemSection) string {
	sorted := slices.Clone(sections)
	slices.SortFunc(sorted, func(a, b SystemSection) int {
		return a.Priority - b.Priority
	})
	var sb strings.Builder
	for i, s := range sorted {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(s.Text)
	}
	return sb.String()
}

// splitSystemCommonTmpl cuts the raw system_common.md template at the two
// anchors that also matter downstream (the bot-identity placeholder and the
// workspace-instructions heading), so the pieces can become independent
// sections instead of being reverse-parsed from the rendered output later.
func splitSystemCommonTmpl(tmpl string) (intro, body, tailBoilerplate string) {
	idxBotInfo := strings.Index(tmpl, botInfoPlaceholder)
	idxWorkspace := strings.Index(tmpl, workspaceHeading)
	if idxBotInfo < 0 || idxWorkspace < 0 {
		panic("agent: system_common.md is missing an expected section anchor")
	}
	intro = tmpl[:idxBotInfo]
	body = strings.TrimSpace(tmpl[idxBotInfo+len(botInfoPlaceholder) : idxWorkspace])
	tailBoilerplate = tmpl[idxWorkspace:]
	return intro, body, tailBoilerplate
}

// buildSystemPromptTail fuses the static workspace-instructions/attachments
// boilerplate with the mode contract and, for main-agent modes, the memory
// section — mirroring the "\n\n" joins GenerateSystemPrompt used to produce
// via raw template concatenation and joinPromptSections.
func buildSystemPromptTail(tailBoilerplate, sessionType string, isSubagent bool) string {
	tail := tailBoilerplate + "\n\n" + modeContractTmpl(sessionType)
	if isSubagent {
		return tail
	}
	return tail + "\n\n" + strings.TrimSpace(includes["_memory"])
}

// modeContractTmpl returns the mode-specific contract text with its trailing
// mainAgentSections/subagentSections placeholder cut off and trimmed.
func modeContractTmpl(sessionType string) string {
	tmpl := selectModeTemplate(sessionType)
	placeholder := "{{mainAgentSections}}"
	if sessionType == sessionmode.Subagent {
		placeholder = "{{subagentSections}}"
	}
	idx := strings.Index(tmpl, placeholder)
	if idx < 0 {
		panic("agent: mode template is missing expected placeholder " + placeholder)
	}
	return strings.TrimSpace(tmpl[:idx])
}
