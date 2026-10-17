package agenttest

import (
	"context"
	"errors"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
)

// UserInput is a user-input service double that records submissions and cancellations.
type UserInput struct {
	Target   userinput.Request
	Resolved userinput.Request

	SubmitCalls       int
	CancelCalls       int
	createCalls       int
	Submitted         userinput.SubmitInput
	Canceled          userinput.CancelInput
	SubmitErr         error
	CancelErr         error
	SubmitHook        func()
	RespondAllowed    bool
	RespondAllowedSet bool
}

func (*UserInput) AdvanceText(context.Context, userinput.AdvanceTextInput) (userinput.AdvanceTextResult, error) {
	return userinput.AdvanceTextResult{}, errors.New("unexpected AdvanceText")
}

func (f *UserInput) CreatePending(context.Context, userinput.CreatePendingInput) (userinput.Request, error) {
	f.createCalls++
	return userinput.Request{}, errors.New("unexpected CreatePending")
}

func (f *UserInput) Get(context.Context, string) (userinput.Request, error) {
	return f.Target, nil
}

func (f *UserInput) ResolveTarget(context.Context, userinput.ResolveInput) (userinput.Request, error) {
	return f.Target, nil
}

func (f *UserInput) Submit(_ context.Context, input userinput.SubmitInput) (userinput.Request, error) {
	f.SubmitCalls++
	f.Submitted = input
	if f.SubmitHook != nil {
		f.SubmitHook()
	}
	if f.SubmitErr != nil {
		return userinput.Request{}, f.SubmitErr
	}
	return f.Resolved, nil
}

func (f *UserInput) Cancel(_ context.Context, input userinput.CancelInput) (userinput.Request, error) {
	f.CancelCalls++
	f.Canceled = input
	if f.CancelErr != nil {
		return userinput.Request{}, f.CancelErr
	}
	return f.Resolved, nil
}

func (f *UserInput) CanRespond(req userinput.Request) bool {
	if f.RespondAllowedSet {
		return f.RespondAllowed
	}
	// Default mirrors a live waiter: a pending row can accept a response.
	return req.Status == userinput.StatusPending
}
