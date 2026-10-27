package agentprocess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/felinics/memoh/internal/workspace/bridge"
	pb "github.com/felinics/memoh/internal/workspace/bridgepb"
	"github.com/felinics/memoh/internal/workspace/bridgesvc"
)

func processTestClient(t *testing.T, service pb.ContainerServiceServer) *bridge.Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bridge requires /bin/sh")
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	if service == nil {
		service = bridgesvc.New(bridgesvc.Options{DefaultWorkDir: t.TempDir(), AllowHostAbsolute: true})
	}
	pb.RegisterContainerServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough://bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return bridge.NewClientFromConn(conn)
}

func TestProcessStdinEOFDrainsOutputAndReportsExit(t *testing.T) {
	client := processTestClient(t, nil)
	for _, code := range []string{"0", "3"} {
		t.Run(code, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			proc, err := Start(ctx, client, "while read line; do :; done; printf flushed; exit "+code, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = proc.Close() }()
			if _, err := proc.Write([]byte("input\n")); err != nil {
				t.Fatal(err)
			}
			proc.CloseStdin()
			output, err := io.ReadAll(proc)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-proc.Done():
			case <-ctx.Done():
				t.Fatal("stdin EOF never reached the process")
			}
			if string(output) != "flushed" || (proc.Err() == nil) != (code == "0") {
				t.Fatalf("output %q, exit error %v", output, proc.Err())
			}
		})
	}
}

type exitlessProcessServer struct {
	pb.UnimplementedContainerServiceServer
}

func (exitlessProcessServer) Exec(stream pb.ContainerService_ExecServer) error {
	_, err := stream.Recv()
	return err
}

func TestProcessTransportEOFDoesNotConfirmTermination(t *testing.T) {
	client := processTestClient(t, exitlessProcessServer{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	proc, err := Start(ctx, client, "unused", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	<-proc.Done()
	if err := proc.Stop(ctx); !errors.Is(err, ErrTerminationUnconfirmed) {
		t.Fatalf("transport EOF confirmed exit: %v", err)
	}
}

func TestExecutionScopeCleansDetachedDescendantsAndPreservesSiblings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process scope")
	}
	client := processTestClient(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	scope := ExecutionScope{ThreadID: uuid.NewString(), RunID: uuid.NewString()}
	siblingScope := ExecutionScope{ThreadID: scope.ThreadID, RunID: uuid.NewString()}
	start := func(scope ExecutionScope, command string) *Process {
		t.Helper()
		proc, err := Start(WithExecutionScope(ctx, scope), client, command, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(proc)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.Copy(io.Discard, reader) }()
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = proc.Stop(stopCtx)
			_ = proc.Close()
		})
		return proc
	}
	pidPath := t.TempDir() + "/descendant.pid"
	child := start(scope, fmt.Sprintf("setsid /bin/sh -c 'echo $$ > %s; exec sleep 300' </dev/null >/dev/null 2>&1 & echo ready; while read line; do :; done", pidPath))
	sibling := start(siblingScope, "echo ready; while read line; do :; done")
	var pid int
	for pid == 0 {
		raw, _ := os.ReadFile(pidPath) //nolint:gosec // PID fixture owned by this test
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if ctx.Err() != nil {
			t.Fatal("detached child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
		}
	})
	if err := child.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		t.Fatalf("probe did not detach: %v", err)
	}
	if err := scope.Reconcile(ctx, client); err != nil {
		t.Fatal(err)
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && !strings.Contains(string(stat), ") Z ") {
		t.Fatal("detached child can still execute after reconciliation")
	}
	select {
	case <-sibling.Done():
		t.Fatal("cleanup stopped a different run")
	default:
	}
}
