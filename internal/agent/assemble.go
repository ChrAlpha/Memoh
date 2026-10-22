package agent

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/plan"
	"github.com/felinics/memoh/internal/agent/resume"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/schedule"
	"github.com/felinics/memoh/internal/agent/subagent"
	"github.com/felinics/memoh/internal/agent/title"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/agent/web"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/providers"
)

// wiring lets this package's tests change what a component is built from.
// New passes the zero value, which changes nothing.
type wiring struct {
	turnAdmitter runreq.TurnAdmitter
	decide       func(*decide.Deps)
	lifecycle    func(*lifecycle.Deps)
	compact      func(*compact.Deps)
	runconfig    func(*runconfig.Deps)
	persist      func(*persist.Deps)
	drive        func(*drive.Deps)
	externalrun  func(*externalrun.Deps)
	title        func(*title.Deps)
	plan         func(*plan.Deps)
	pending      func(*pending.Deps)
	answer       func(*answer.Deps)
}

// adjusted applies a wiring change to the Deps of one component.
func adjusted[D any](d D, change func(*D)) D {
	if change != nil {
		change(&d)
	}
	return d
}

func assemble(d Deps, w wiring) (*Service, error) {
	teamScopes := d.TeamScopes
	if teamScopes == nil {
		teamScopes = runreq.SingleTeam(d.AllowedTeam)
	}
	var admitter runreq.TurnAdmitter = d.SessionRuntime
	if w.turnAdmitter != nil {
		admitter = w.turnAdmitter
	}
	log := func(name string) *slog.Logger { return d.Logger.With(slog.String("service", "agent/"+name)) }

	service := &Service{allowedTeam: d.AllowedTeam, activeTurns: &runreq.ActiveTurnTracker{}}
	service.activePrompts = activeprompt.New()
	service.decider = decide.New(adjusted(decide.Deps{
		Logger:         log("decide"),
		Queries:        d.Queries,
		Sessions:       d.Sessions,
		ToolApproval:   d.ToolApproval,
		UserInput:      d.UserInput,
		BotPermissions: d.BotPermissions,
		ActivePrompts:  service.activePrompts,
		Events:         d.SessionRuntime,
		Limits:         d.Agent,
	}, w.decide))
	service.botenv = botenv.New(botenv.Deps{
		Logger:        log("botenv"),
		Queries:       d.Queries,
		Settings:      d.Settings,
		Accounts:      d.Accounts,
		Bridge:        d.Agent.BridgeProvider(),
		Hooks:         d.Hooks,
		ClockLocation: d.ClockLocation,
	})
	service.transcript = transcript.New(transcript.Deps{
		Logger:         log("transcript"),
		Messages:       d.Messages,
		Queries:        d.Queries,
		Sessions:       d.Sessions,
		FallbackTokens: d.ContextFallbackTokens,
	})
	service.lifecycle = lifecycle.New(adjusted(lifecycle.Deps{
		Store:     d.Queries,
		Runs:      d.Queries,
		Decisions: service.decider,
		Logger:    log("lifecycle"),
	}, w.lifecycle))
	service.memory = memory.New(memory.Deps{
		Logger:     log("memory"),
		Settings:   d.Settings,
		Messages:   d.Messages,
		Bot:        service.botenv,
		Transcript: service.transcript,
		Registry:   d.Memory,
	})
	service.overview = memory.NewOverview(memory.OverviewDeps{
		Logger: log("memory"),
		Store:  d.Queries,
		Keeper: service.memory,
	})
	// A typed nil stored in an interface would read as configured.
	var runner compact.Runner
	if d.Compaction != nil {
		runner = d.Compaction
	}
	service.compact = compact.New(adjusted(compact.Deps{
		Logger:     log("compact"),
		Settings:   d.Settings,
		Models:     d.Models,
		Queries:    d.Queries,
		Transcript: service.transcript,
		Runner:     runner,
	}, w.compact))
	service.runconfig = runconfig.New(adjusted(runconfig.Deps{
		Logger:             log("runconfig"),
		Queries:            d.Queries,
		Credentials:        providers.NewService(nil, d.Queries, ""),
		Models:             d.Models,
		Sessions:           d.Sessions,
		Agent:              d.Agent,
		Bot:                service.botenv,
		Transcript:         service.transcript,
		Memory:             service.memory,
		MemoryOverview:     service.overview,
		Compact:            service.compact,
		Decider:            service.decider,
		BotPermissions:     d.BotPermissions,
		WorkspaceTargets:   d.WorkspaceTargets,
		Skills:             d.Skills,
		Assets:             d.Assets,
		PlatformIdentities: d.PlatformIdentities,
		Workdirs:           d.Workdirs,
		Background:         d.Background,
	}, w.runconfig))
	// A typed nil stored in an interface would read as a queue.
	var queue pending.Store
	var promoted persist.PromotedInputs
	if d.QueuedInputs != nil {
		queue = d.QueuedInputs
		promoted = queue
	}
	service.persist = persist.New(adjusted(persist.Deps{
		Logger:   log("persist"),
		Messages: d.Messages,
		Sessions: d.Sessions,
		Bot:      service.botenv,
		Runtime:  d.SessionRuntime,
		Rounds:   service.memory,
		Commits:  service.compact,
		Promoted: promoted,
	}, w.persist))
	service.signer = admit.NewSigner(d.RunCredentialSecret)
	service.admit = admit.New(admit.Deps{Runtime: admitter, Queries: d.Queries, Signer: service.signer})
	service.driver = drive.New(adjusted(drive.Deps{
		Logger:       log("drive"),
		Runtime:      admitter,
		Events:       d.SessionRuntime,
		Lifecycle:    service.lifecycle,
		Persist:      service.persist,
		OutputMedia:  d.OutputMedia,
		OutputSpeech: d.OutputSpeech,
	}, w.drive))
	// The message store is the only writer of a spawned run's history; Deps
	// validation has already required it to write fenced steps.
	steps, _ := d.Messages.(messagepkg.AgentStepPersister)
	var err error
	service.subagent, err = subagent.New(subagent.Deps{
		Logger:      log("subagent"),
		Admit:       service.admit,
		Driver:      service.driver,
		Lifecycle:   service.lifecycle,
		Compact:     service.compact,
		ActiveTurns: service.activeTurns,
		Events:      d.SessionRuntime,
		Steps:       steps,
	})
	if err != nil {
		return nil, err
	}
	service.externalrun = externalrun.New(adjusted(externalrun.Deps{
		Logger:         log("externalrun"),
		Store:          d.Queries,
		Sessions:       d.Sessions,
		Messages:       d.Messages,
		BotPermissions: d.BotPermissions,
		Admitter:       admitter,
		Aborter:        d.SessionRuntime,
		Runtime:        d.SessionRuntime,
		Persist:        service.persist,
		Memory:         service.memory,
		Runconfig:      service.runconfig,
		Decider:        service.decider,
		ActivePrompts:  service.activePrompts,
		Driver:         service.driver,
		Lifecycle:      service.lifecycle,
		Drivers:        d.ExternalRuntimes,
		AllowedTeam:    d.AllowedTeam,
	}, w.externalrun))
	service.title = title.New(adjusted(title.Deps{
		Logger:      log("title"),
		Store:       d.Queries,
		Sessions:    d.Sessions,
		Accounts:    d.Accounts,
		Models:      service.runconfig,
		Memory:      service.memory,
		Credentials: providers.NewService(nil, d.Queries, ""),
		Events:      d.Events,
	}, w.title))
	service.plan = plan.New(adjusted(plan.Deps{
		Logger:     log("plan"),
		Store:      d.Queries,
		Agent:      d.Agent,
		Runtime:    d.SessionRuntime,
		Executions: plan.NewApprovalExecutions(d.Queries),
		Admit:      service.admit,
		Bot:        service.botenv,
		Transcript: service.transcript,
		Lifecycle:  service.lifecycle,
		Persist:    service.persist,
		Compact:    service.compact,
		Runconfig:  service.runconfig,
		External:   service.externalrun,
		Title:      service.title,
	}, w.plan))
	service.continuer = resume.NewContinuer(resume.ContinuerDeps{
		Logger:         log("resume"),
		Queries:        d.Queries,
		Accounts:       d.Accounts,
		BotPermissions: d.BotPermissions,
		Signer:         service.signer,
		Admit:          service.admit,
		Decider:        service.decider,
		Transcript:     service.transcript,
		Driver:         service.driver,
		Plan:           service.plan,
		TeamScopes:     teamScopes,
	})
	service.pending = pending.New(adjusted(pending.Deps{
		Logger:      log("pending"),
		Queries:     d.Queries,
		Queue:       queue,
		Runtime:     d.SessionRuntime,
		Turns:       service,
		Admit:       service.admit,
		Signer:      service.signer,
		ActiveTurns: service.activeTurns,
		Driver:      service.driver,
		Plan:        service.plan,
		Transcript:  service.transcript,
		Bot:         service.botenv,
		Persist:     service.persist,
		Messages:    d.Messages,
		TeamScopes:  teamScopes,

		Continuations:      service.continuer,
		WorkspaceReadiness: d.WorkspaceReadiness,
	}, w.pending))
	service.resume = resume.NewScanner(resume.ScannerDeps{
		Logger:     log("resume"),
		Queries:    d.Queries,
		Pending:    service.pending,
		TeamScopes: teamScopes,
	})
	service.discussSweep = pending.NewDiscussSweep(service.pending)
	service.queueSweep = pending.NewQueueSweep(service.pending)
	service.web = web.New(web.Deps{
		Runtime:     admitter,
		Admit:       service.admit,
		ActiveTurns: service.activeTurns,
		Pending:     service.pending,
		Compact:     service.compact,
		Driver:      service.driver,
		Plan:        service.plan,
		Messages:    d.Messages,
		Persist:     service.persist,
		Runconfig:   service.runconfig,
		Bot:         service.botenv,
	})
	service.schedule = schedule.New(schedule.Deps{
		Admit:       service.admit,
		ActiveTurns: service.activeTurns,
		ExternalRun: service.externalrun,
		Driver:      service.driver,
		Plan:        service.plan,
	})
	service.answer = answer.New(adjusted(answer.Deps{
		Logger:     log("answer"),
		Decider:    service.decider,
		Runtime:    d.SessionRuntime,
		Driver:     service.driver,
		Transcript: service.transcript,
		Continuer:  service.plan,
	}, w.answer))
	bindSessionRuntime(d.SessionRuntime, d.Logger.With(slog.String("service", "agent")), service)
	return service, nil
}

// bindSessionRuntime registers the callbacks the runtime invokes into the
// service's components. The manager is built before the service and holds no
// reference back to it, so New can wire both ways.
func bindSessionRuntime(manager *sessionruntime.Manager, logger *slog.Logger, s *Service) {
	manager.SetDecisionStore(s.decider)
	manager.SetLostRunDecisionCanceller(s.decider.CancelLostRunDecisions)
	manager.SetCommandHandler(func(ctx context.Context, command sessionruntime.Command) error {
		return s.answer.HandleCommand(ctx, command)
	})
	manager.SetDecisionFinalizer(s.decider.FinalizeRuntimeDecisions)
	manager.SetTerminalObserver(terminalFanout(
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			outcome.LogRunResult(ctx, logger, terminal)
		},
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			s.lifecycle.ReconcileTerminal(ctx, terminal)
		},
		func(_ context.Context, terminal sessionruntime.TerminalRun) {
			s.overview.RunEnded(terminal.BotID, terminal.SessionID)
		},
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			s.pending.AfterSessionTerminal(ctx, terminal)
		},
	))
	manager.SetTerminalReconciler(func(ctx context.Context) error {
		return s.lifecycle.ReconcileTerminals(ctx)
	})
}

// terminalFanout runs the terminal observers in the order given. The order is
// the contract: the outcome record comes first, the lifecycle reconciliation
// next, the memory overview's note that the session's prompt cache was just
// refreshed after that, and whatever the run's end releases (queued input)
// last.
func terminalFanout(steps ...func(context.Context, sessionruntime.TerminalRun)) func(context.Context, sessionruntime.TerminalRun) {
	return func(ctx context.Context, terminal sessionruntime.TerminalRun) {
		for _, step := range steps {
			step(ctx, terminal)
		}
	}
}
