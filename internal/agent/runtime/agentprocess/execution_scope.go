package agentprocess

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/workspace/bridge"
)

// ExecutionScope names exactly one child execution, independently of its
// parent and siblings sharing the workspace. It is not a filesystem sandbox.
type (
	ExecutionScope    struct{ ThreadID, RunID string }
	executionScopeKey struct{}
)

func WithExecutionScope(ctx context.Context, scope ExecutionScope) context.Context {
	return context.WithValue(ctx, executionScopeKey{}, scope)
}

func ExecutionScopeFromContext(ctx context.Context) ExecutionScope {
	scope, _ := ctx.Value(executionScopeKey{}).(ExecutionScope)
	return scope
}

func (s ExecutionScope) Env() []string {
	if s.ThreadID == "" || s.RunID == "" {
		return nil
	}
	return []string{"MEMOH_CHILD_EXECUTION=" + s.ThreadID + ":" + s.RunID}
}

// Reconcile stops the scope's remaining marked process trees. It fails closed
// on transport errors and unexpected output; see the scan's limitations below.
func (s ExecutionScope) Reconcile(ctx context.Context, client *bridge.Client) error {
	if _, err := uuid.Parse(s.ThreadID); err != nil {
		return err
	}
	if _, err := uuid.Parse(s.RunID); err != nil {
		return err
	}
	result, err := client.Exec(ctx, fmt.Sprintf(executionProcessScript, s.Env()[0]), "/", 15)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "0" {
		return ErrTerminationUnconfirmed
	}
	return nil
}

// executionProcessScript SIGKILLs every workspace process whose environment
// holds the marker, together with its descendants, until a scan finds none,
// and prints how many the last scan found. The tree is stopped before it is
// killed so that a child forked meanwhile is still found under its parent,
// which covers children that reset their environment; a tree that keeps
// growing through every round is reported unsettled. An exited process reads
// empty and stops counting. Not seen: a process that made itself non-dumpable
// (its environ needs CAP_SYS_PTRACE), and one that reset its environment
// outside the tree, such as a daemon or a marked process that ran exec env -i.
// A grep without -z would read every process as unmarked, and so would an
// unreadable /proc, so the script refuses to run with either.
const executionProcessScript = `m='%s'
printf '%%s\0' "$m" | grep -qzxF -- "$m" || { echo 'grep -z unsupported'; exit 3; }
[ -r /proc/$$/environ ] || { echo '/proc unreadable'; exit 3; }
grow() {
	pairs=
	for s in /proc/[0-9]*/status; do
		p=${s#/proc/}; p=${p%%/status}
		while IFS=: read -r k v; do [ "$k" = PPid ] && { pairs="$pairs $p:$((v))"; break; }; done 2>/dev/null <"$s"
	done
	grown=1
	while [ "$grown" = 1 ]; do
		grown=0
		for pair in $pairs; do
			case $tree in *" ${pair#*:} "*) case $tree in *" ${pair%%:*} "*) ;; *) tree="$tree${pair%%:*} "; grown=1 ;; esac ;; esac
		done
	done
}
left=0
for i in 1 2 3 4 5 6 7 8 9 10; do
	tree=' '
	for e in /proc/[0-9]*/environ; do
		p=${e#/proc/}; p=${p%%/environ}
		grep -qzxF -- "$m" "$e" 2>/dev/null && tree="$tree$p "
	done
	left=0
	[ "$tree" = ' ' ] && break
	settled=
	for r in 1 2 3 4 5 6 7 8 9 10; do
		kill -STOP $tree 2>/dev/null
		before=$tree
		grow
		[ "$tree" = "$before" ] && { settled=1; break; }
	done
	kill -KILL $tree 2>/dev/null
	[ -n "$settled" ] || { echo unsettled; exit 0; }
	set -- $tree
	left=$#
	sleep 0.2
done
echo "$left"`
