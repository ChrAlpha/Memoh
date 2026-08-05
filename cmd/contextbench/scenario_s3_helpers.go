package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/contextview"
)

type s3InitialContext struct {
	systemFrags []contextfrag.ContextFrag
	sourceFrags []contextfrag.ContextFrag
	messages    []sdk.Message
}

func buildS3InitialContext(fixture benchFixture) s3InitialContext {
	systemFrags := make([]contextfrag.ContextFrag, 0, 4)
	for _, frag := range fixture.systemFrags {
		if frag.RetentionTier == contextfrag.RetentionRequired || frag.Kind == contextfrag.KindBotIdentity {
			systemFrags = append(systemFrags, frag)
		}
	}

	messageFrags := fixture.sourceFrags[len(fixture.systemFrags):]
	if len(messageFrags) != len(fixture.messages) {
		panic("build S3 initial context: message fragments do not match messages")
	}
	messageStart := max(0, len(fixture.messages)-s3InitialMessageCount)
	messages := cloneMessages(fixture.messages[messageStart:])
	sourceFrags := make([]contextfrag.ContextFrag, 0, len(systemFrags)+len(messages))
	sourceFrags = append(sourceFrags, systemFrags...)
	sourceFrags = append(sourceFrags, messageFrags[messageStart:]...)
	return s3InitialContext{systemFrags: systemFrags, sourceFrags: sourceFrags, messages: messages}
}

func buildS3Steps() []s3Step {
	rng := rand.New(rand.NewSource(fixtureSeed + 3)) //nolint:gosec // fixed benchmark seed
	hugeSteps := make(map[int]bool, s3HugeResultCount)
	for _, index := range rng.Perm(s3StepCount)[:s3HugeResultCount] {
		hugeSteps[index+1] = true
	}
	imageSteps := make(map[int]bool, s3ImageStepCount)
	for _, index := range rng.Perm(s3StepCount)[:s3ImageStepCount] {
		imageSteps[index+1] = true
	}

	steps := make([]s3Step, s3StepCount)
	for index := range steps {
		stepNumber := index + 1
		huge := hugeSteps[stepNumber]
		resultBytes := 0
		if huge {
			resultBytes = 50*1_024 + rng.Intn(100*1_024+1)
		} else {
			sample := math.Exp(math.Log(4*1_024) + 0.9*rng.NormFloat64())
			resultBytes = max(256, min(48*1_024, int(math.Round(sample))))
		}
		injection := ""
		switch stepNumber {
		case 20, 45, 70:
			injection = fmt.Sprintf(
				"<message sender=\"contextbench\" step=\"%s\">S3_INJECT_%s</message>",
				threeDigits(stepNumber),
				threeDigits(stepNumber),
			)
		}
		backgroundRevision := stepNumber / 5
		backgroundSummary := ""
		backgroundRefresh := stepNumber%5 == 0
		if backgroundRevision > 0 {
			backgroundSummary = s3BackgroundSummary(backgroundRevision)
		}
		toolName := "exec"
		if imageSteps[stepNumber] {
			toolName = "read_media"
		}
		steps[index] = s3Step{
			Step:                  stepNumber,
			ToolName:              toolName,
			ToolResult:            s3SizedResult(stepNumber, resultBytes),
			ToolResultBytes:       resultBytes,
			HugeResult:            huge,
			Image:                 imageSteps[stepNumber],
			Injection:             injection,
			BackgroundRefresh:     backgroundRefresh,
			BackgroundRevision:    backgroundRevision,
			BackgroundSummaryText: backgroundSummary,
		}
	}
	return steps
}

func advanceS3Messages(messages []sdk.Message, prefixCount int, step s3Step, replaceBackground bool) []sdk.Message {
	callID := "contextbench-s3-" + threeDigits(step.Step)
	next := cloneMessages(messages)
	next = append(next,
		sdk.Message{
			Role: sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.ToolCallPart{
				ToolCallID: callID,
				ToolName:   step.ToolName,
				Input: map[string]any{
					"result_bytes": step.ToolResultBytes,
					"step":         step.Step,
				},
			}},
		},
		sdk.ToolMessage(sdk.ToolResultPart{
			ToolCallID: callID,
			ToolName:   step.ToolName,
			Result:     step.ToolResult,
		}),
	)
	if step.Image {
		next = append(next, sdk.Message{
			Role: sdk.MessageRoleUser,
			Content: []sdk.MessagePart{sdk.ImagePart{
				Image:     "data:image/png;base64," + strings.Repeat("A", s3ImagePayloadBytes),
				MediaType: "image/png",
			}},
		})
	}
	if step.Injection != "" {
		next = append(next, sdk.UserMessage(step.Injection))
	}
	if step.BackgroundRefresh {
		if replaceBackground {
			next = removeS3BackgroundSummaries(next, prefixCount)
		}
		next = append(next, sdk.UserMessage(contextfrag.BackgroundSummaryMessagePrefix+step.BackgroundSummaryText))
	}
	return next
}

func removeS3BackgroundSummaries(messages []sdk.Message, prefixCount int) []sdk.Message {
	prefixCount = max(0, min(prefixCount, len(messages)))
	out := make([]sdk.Message, 0, len(messages))
	out = append(out, cloneMessages(messages[:prefixCount])...)
	for _, message := range messages[prefixCount:] {
		if contextfrag.IsBackgroundSummaryCarrier(message) {
			continue
		}
		out = append(out, cloneMessages([]sdk.Message{message})[0])
	}
	return out
}

func recoverS3AfterFatal(prefix []sdk.Message, injections []string, backgroundSummary string) []sdk.Message {
	messages := cloneMessages(prefix)
	for _, injection := range injections {
		messages = append(messages, sdk.UserMessage(injection))
	}
	if backgroundSummary != "" {
		messages = append(messages, sdk.UserMessage(contextfrag.BackgroundSummaryMessagePrefix+backgroundSummary))
	}
	return messages
}

func s3SizedResult(step, size int) string {
	prefix := fmt.Sprintf("step=%s ", threeDigits(step))
	if size <= len(prefix) {
		return prefix[:size]
	}
	fill := string(rune('a' + step%26))
	return prefix + strings.Repeat(fill, size-len(prefix))
}

func s3BackgroundSummary(revision int) string {
	return fmt.Sprintf("contextbench running tasks revision=%s status=active", threeDigits(revision))
}

func countS3InjectedMessages(messages []sdk.Message, expected []string) int {
	present := 0
	for _, marker := range expected {
		if s3HasExactUserText(messages, marker) {
			present++
		}
	}
	return present
}

func s3HasExactUserText(messages []sdk.Message, expected string) bool {
	for _, message := range messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			text, ok := part.(sdk.TextPart)
			if ok && text.Text == expected {
				return true
			}
		}
	}
	return false
}

func s3BackgroundSummaryStatus(messages []sdk.Message, expected string) (count int, current bool) {
	want := contextfrag.BackgroundSummaryMessagePrefix + expected
	for _, message := range messages {
		if !contextfrag.IsBackgroundSummaryCarrier(message) {
			continue
		}
		count++
		text := message.Content[0].(sdk.TextPart)
		if expected != "" && text.Text == want {
			current = true
		}
	}
	return count, current
}

func countS3TrimNotices(messages []sdk.Message) int {
	count := 0
	for _, message := range messages {
		for _, part := range message.Content {
			text, ok := part.(sdk.TextPart)
			if ok && text.Text == contextview.HistoryTrimNotice {
				count++
			}
		}
	}
	return count
}

func countS3ImageParts(messages []sdk.Message) int {
	count := 0
	for _, message := range messages {
		for _, part := range message.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				count++
			}
		}
	}
	return count
}

func s3PrefixIntact(messages, prefix []sdk.Message) bool {
	if len(messages) < len(prefix) {
		return false
	}
	for index := range prefix {
		if !equalMessages(messages[index], prefix[index]) {
			return false
		}
	}
	return true
}

func s3ToolClosureValid(messages []sdk.Message) bool {
	pending := make(map[string]struct{})
	for _, message := range messages {
		switch message.Role {
		case sdk.MessageRoleAssistant:
			if len(pending) > 0 {
				return false
			}
			for _, part := range message.Content {
				call, ok := part.(sdk.ToolCallPart)
				if !ok {
					continue
				}
				callID := strings.TrimSpace(call.ToolCallID)
				if callID == "" {
					return false
				}
				if _, duplicate := pending[callID]; duplicate {
					return false
				}
				pending[callID] = struct{}{}
			}
		case sdk.MessageRoleTool:
			if len(message.Content) == 0 {
				return false
			}
			for _, part := range message.Content {
				result, ok := part.(sdk.ToolResultPart)
				if !ok {
					return false
				}
				callID := strings.TrimSpace(result.ToolCallID)
				if _, ok := pending[callID]; !ok {
					return false
				}
				delete(pending, callID)
			}
		default:
			if len(pending) > 0 {
				return false
			}
		}
	}
	return len(pending) == 0
}

func cloneS3Counts(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
