package application

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/team"
)

// newSweep is a sweep over the process's own service, with the singleton
// scope the self-hosted deployment has.
func newSweep(proc *discussProcess) *pending.DiscussSweep {
	setAllowedTeam(proc.svc, team.DefaultTeamID)
	return pending.NewDiscussSweep(proc.svc.pending)
}

// The sweep runs every minute and looks half an hour back.
func TestDiscussSweepConstants(t *testing.T) {
	if pending.DiscussSweepInterval != time.Minute {
		t.Fatalf("pending.DiscussSweepInterval = %v, want 1m", pending.DiscussSweepInterval)
	}
	if pending.DiscussSweepWindow <= pending.DiscussSweepInterval {
		t.Fatalf("pending.DiscussSweepWindow = %v, want longer than the interval", pending.DiscussSweepWindow)
	}
}

// Input that arrived while nothing admitted it is answered by the next sweep,
// once. Each call of sweep is one interval's worth.
func TestDiscussSweepAnswersInputWithinOneInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{send: "caught up"}, &discussStep{})
	sweep := newSweep(proc)
	ctx := context.Background()

	sweep.Sweep(ctx, time.Now())
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "nobody woke the server for this"})
	if got := len(env.runs(t)); got != 0 {
		t.Fatalf("runs before the sweep = %d, want 0", got)
	}

	sweep.Sweep(ctx, time.Now())
	env.waitRunState(t, env.invocation(hw), "completed")
	if got := env.cover(t); got != hw {
		t.Fatalf("cover = %d, want %d", got, hw)
	}
	if sent := proc.sender.delivered(); len(sent) != 1 {
		t.Fatalf("delivered = %+v, want one send", sent)
	}

	// Later sweeps find the input answered and leave the session alone.
	sweep.Sweep(ctx, time.Now())
	sweep.Sweep(ctx, time.Now())
	if got := len(env.runs(t)); got != 1 {
		t.Fatalf("runs after further sweeps = %d, want 1", got)
	}
}

// The sweep ends at its window: input older than that is left alone, and the
// session answers it when something newer arrives.
func TestDiscussSweepLeavesInputOlderThanTheWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{send: "late"}, &discussStep{})
	sweep := newSweep(proc)
	ctx := context.Background()

	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "a while ago"})
	written := time.Now()
	sweep.Sweep(ctx, written.Add(pending.DiscussSweepWindow+time.Minute))
	if got := len(env.runs(t)); got != 0 {
		t.Fatalf("runs for input older than the window = %d, want 0", got)
	}

	// Inside the window the same input is found.
	sweep.Sweep(ctx, written.Add(pending.DiscussSweepWindow-time.Minute))
	env.waitRunState(t, env.invocation(hw), "completed")
}

// The sweep visits every database scope the provider names, page after page.
func TestDiscussSweepVisitsEveryScope(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc := newDiscussProcess(t, env)
	scopes := &pagedScopes{scopes: []string{"scope-a", "scope-b", "scope-c"}}
	useTeamScopes(proc.svc, scopes)
	pending.NewDiscussSweep(proc.svc.pending).Sweep(context.Background(), time.Now())
	if got := scopes.visited(); len(got) != 3 || got[0] != "scope-a" || got[2] != "scope-c" {
		t.Fatalf("scopes visited = %v, want all three in order", got)
	}
}

// The loop sweeps once when it starts and once for every tick, and ends with
// its context.
func TestDiscussSweepRunSweepsAtStartAndOnEveryTick(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc := newDiscussProcess(t, env)
	scopes := &pagedScopes{scopes: []string{"scope-a"}}
	useTeamScopes(proc.svc, scopes)
	sweep := pending.NewDiscussSweep(proc.svc.pending)

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweep.Run(ctx, ticks, time.Now)
	}()
	waitSweeps := func(want int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for len(scopes.visited()) < want {
			if time.Now().After(deadline) {
				t.Fatalf("sweeps = %d, want %d", len(scopes.visited()), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitSweeps(1)
	ticks <- time.Now()
	waitSweeps(2)
	ticks <- time.Now()
	waitSweeps(3)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not end with its context")
	}
	if got := len(scopes.visited()); got != 3 {
		t.Fatalf("sweeps = %d, want 3", got)
	}
}

// pagedScopes names its scopes one to a page.
type pagedScopes struct {
	scopes []string
	mu     sync.Mutex
	bound  []string
}

type pagedScopeKey struct{}

func (*pagedScopes) CurrentScope(ctx context.Context) string {
	scope, _ := ctx.Value(pagedScopeKey{}).(string)
	return scope
}

func (p *pagedScopes) ListScopePage(_ context.Context, after string, _ int32) (runreq.TeamScopePage, error) {
	next := 0
	if after != "" {
		n, err := strconv.Atoi(after)
		if err != nil {
			return runreq.TeamScopePage{}, err
		}
		next = n
	}
	page := runreq.TeamScopePage{Scopes: []string{p.scopes[next]}}
	if next+1 >= len(p.scopes) {
		page.Complete = true
		return page, nil
	}
	page.NextCursor = strconv.Itoa(next + 1)
	return page, nil
}

func (p *pagedScopes) BindScope(ctx context.Context, scope string) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound = append(p.bound, scope)
	return context.WithValue(ctx, pagedScopeKey{}, scope)
}

func (p *pagedScopes) visited() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bound...)
}
