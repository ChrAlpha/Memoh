package drivetest

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// ScheduledSpec is the schedule entry's spec over exec: an admitted run nobody
// waits on, whose input is synthetic.
func ScheduledSpec(exec drive.RunExecutor, delivery agentapi.RunDelivery) drive.RunSpec {
	return drive.RunSpec{
		Run: drive.AdmittedRun{
			Handle: sessionruntime.RunHandle{
				BotID: "bot-1", SessionID: "session-1", RunID: "run-1",
				TurnID: "turn-1", FencingToken: 1,
			},
			TurnID: "turn-1",
			Mode:   "schedule",
		},
		Req:        agenttest.TriggerRequest(),
		Delivery:   delivery,
		Input:      outcome.InputSynthetic,
		Unattended: true,
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{Exec: exec, Req: agenttest.TriggerRequest()}, nil
		},
	}
}
