package native

import (
	"reflect"
	"sync"

	sdk "github.com/memohai/twilight-ai/sdk"
)

// stepMessageCapture retains context messages appended by PrepareStep. They
// are input to the next model call, so they become durable only with that
// call's complete step.
type stepMessageCapture struct {
	mu          sync.Mutex
	byStep      map[int][]sdk.Message
	prepared    map[int]preparedMessageCapture
	nextStep    int
	lastStep    int
	onReconcile []func(int, []admittedPreparedMessage)
}

type preparedMessageSpan struct {
	start int
	end   int
}

type preparedMessageCapture struct {
	messages []sdk.Message
	span     preparedMessageSpan
}

type admittedPreparedMessage struct {
	index int
}

func (c *stepMessageCapture) messages(stepIndex int) []sdk.Message {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneProviderMessages(c.byStep[stepIndex])
}

func capturePreparedStepMessages(prepare func(*sdk.GenerateParams) *sdk.GenerateParams) (func(*sdk.GenerateParams) *sdk.GenerateParams, *stepMessageCapture) {
	capture := &stepMessageCapture{
		byStep:   make(map[int][]sdk.Message),
		prepared: make(map[int]preparedMessageCapture),
		nextStep: 1, // Twilight calls PrepareStep only before steps after step zero.
	}
	if prepare == nil {
		return nil, capture
	}
	return func(params *sdk.GenerateParams) *sdk.GenerateParams {
		before := len(params.Messages)
		override := prepare(params)
		actual := params
		if override != nil {
			actual = override
		}
		capture.mu.Lock()
		step := capture.nextStep
		capture.nextStep++
		capture.lastStep = step
		delete(capture.byStep, step)
		delete(capture.prepared, step)
		if actual != nil && len(actual.Messages) > before {
			capture.byStep[step] = cloneProviderMessages(actual.Messages[before:])
			capture.prepared[step] = preparedMessageCapture{
				messages: cloneProviderMessages(actual.Messages),
				span:     preparedMessageSpan{start: before, end: len(actual.Messages)},
			}
		}
		capture.mu.Unlock()
		return override
	}, capture
}

func (c *stepMessageCapture) addAdmissionObserver(observer func(int, []admittedPreparedMessage)) {
	if c == nil || observer == nil {
		return
	}
	c.mu.Lock()
	c.onReconcile = append(c.onReconcile, observer)
	c.mu.Unlock()
}

// reconcileLast makes the latest raw PrepareStep additions match the
// authoritative provider preflight. It is deliberately repeatable: the first
// preflight of a retry can revise the admission decision made by the failed
// original attempt. Hook and background carriers are outside this capture and
// remain transient.
func (c *stepMessageCapture) reconcileLast(admitted []sdk.Message) {
	if c == nil {
		return
	}
	c.mu.Lock()
	step := c.lastStep
	captured, ok := c.prepared[step]
	observers := append([]func(int, []admittedPreparedMessage){}, c.onReconcile...)
	c.mu.Unlock()
	if !ok {
		return
	}

	retained, admissions := retainedPreparedMessages(captured.messages, admitted, captured.span)
	c.mu.Lock()
	if c.lastStep != step {
		c.mu.Unlock()
		return
	}
	if len(retained) == 0 {
		delete(c.byStep, step)
	} else {
		c.byStep[step] = cloneProviderMessages(retained)
	}
	c.mu.Unlock()
	for _, observer := range observers {
		observer(step, clonePreparedAdmissions(admissions))
	}
}

func retainedPreparedMessages(prepared, admitted []sdk.Message, span preparedMessageSpan) ([]sdk.Message, []admittedPreparedMessage) {
	if span.start < 0 || span.end > len(prepared) || span.start >= span.end {
		return nil, nil
	}
	preparedComparable := cloneProviderMessages(prepared)
	admittedComparable := cloneProviderMessages(admitted)
	clearProviderCacheControls(preparedComparable)
	clearProviderCacheControls(admittedComparable)

	retained := make([]bool, len(preparedComparable))
	nextPrepared := 0
	for _, message := range admittedComparable {
		match := -1
		for i := nextPrepared; i < len(preparedComparable); i++ {
			if reflect.DeepEqual(preparedComparable[i], message) {
				match = i
				break
			}
		}
		if match < 0 {
			continue
		}
		retained[match] = true
		nextPrepared = match + 1
	}

	out := make([]sdk.Message, 0, span.end-span.start)
	admissions := make([]admittedPreparedMessage, 0, span.end-span.start)
	for i := span.start; i < span.end; i++ {
		if retained[i] {
			out = append(out, prepared[i])
			admissions = append(admissions, admittedPreparedMessage{index: i})
		}
	}
	return out, admissions
}

func clonePreparedAdmissions(admissions []admittedPreparedMessage) []admittedPreparedMessage {
	cloned := make([]admittedPreparedMessage, len(admissions))
	copy(cloned, admissions)
	return cloned
}

func (c *stepMessageCapture) decorate(stepIndex int, step *sdk.StepResult, metadata *toolExecutionMetadataRegistry) *sdk.StepResult {
	if step == nil {
		return nil
	}
	decorated := *step
	decorated.Messages = append(c.messages(stepIndex), step.Messages...)
	if decorated.DeferredToolApproval != nil {
		decorated.Messages = annotateDeferredApproval(decorated.Messages, *decorated.DeferredToolApproval)
	}
	decorated.Messages = metadata.annotate(decorated.Messages)
	return &decorated
}
