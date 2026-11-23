package pipeline

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
)

const (
	loadCalls       = 4000
	loadConcurrency = 16
)

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// stageSmoke starts the built server and checks it the way a platform would:
// health, reflection, a short load test, memory, and graceful shutdown.
func (x *executor) stageSmoke(ctx context.Context, st *StageState) error {
	model := gen.NewModel(x.spec)
	port, err := freePort()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// The first exec of a new binary pays for OS verification (about 0.3 s on
	// macOS). Pay it with a run that exits at once on bad config, and report it
	// apart from the real startup time.
	rt := &metrics.Runtime{}
	x.rep.Runtime = rt
	warm := exec.Command(x.binary())
	warm.Env = x.m.childEnv(false, "LOG_LEVEL=not-a-level")
	t0 := time.Now()
	_ = warm.Run()
	rt.FirstExecMs = float64(time.Since(t0).Microseconds()) / 1000
	x.log(st, fmt.Sprintf("first exec (exits on purpose, warms OS binary checks): %.1f ms", rt.FirstExecMs))

	// The binary contains LLM-written code: it gets the scrubbed environment.
	env := []string{"GRPC_ADDR=" + addr, "LOG_LEVEL=warn", "SHUTDOWN_TIMEOUT=5s"}
	storeKind := "memory"
	if x.db != nil {
		env = append(env, "DATABASE_URL="+x.db.url)
		storeKind = "postgres"
	}
	rt.Store = storeKind
	cmd := exec.Command(x.binary())
	cmd.Env = x.m.childEnv(false, env...)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	x.log(st, fmt.Sprintf("$ GRPC_ADDR=%s LOG_LEVEL=warn bin/server   (%s store)", addr, storeKind))
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			x.log(st, "server: "+sc.Text())
		}
		exited <- cmd.Wait()
	}()
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			x.log(st, fmt.Sprintf("server stopped by the smoke station: %v", <-exited))
		}
	}()

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 5 * time.Millisecond, Multiplier: 1.5, Jitter: 0.1, MaxDelay: 100 * time.Millisecond},
			MinConnectTimeout: time.Second,
		}))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := waitReady(ctx, conn, addr, model.Health, exited); err != nil {
		return err
	}
	rt.StartupMs = float64(time.Since(start).Microseconds()) / 1000
	x.log(st, fmt.Sprintf("ready after %.1f ms", rt.StartupMs))

	if model.Health {
		resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: model.FullServiceName})
		if err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		rt.HealthStatus = resp.GetStatus().String()
		x.log(st, "health "+model.FullServiceName+": "+rt.HealthStatus)
	}
	if model.Reflection {
		svcs, err := listServices(ctx, conn)
		if err != nil {
			return fmt.Errorf("reflection: %w", err)
		}
		rt.Services = svcs
		x.log(st, "reflection lists: "+strings.Join(svcs, ", "))
		if !slices.Contains(svcs, model.FullServiceName) {
			return fmt.Errorf("reflection does not list %s", model.FullServiceName)
		}
	}

	method := "/grpc.health.v1.Health/Check"
	for _, e := range model.Entities {
		if e.HasList {
			method = "/" + model.FullServiceName + "/List" + e.Plural
			break
		}
	}
	rt.LoadMethod, rt.LoadCalls, rt.LoadConcurrency = method, loadCalls, loadConcurrency
	lat, errs, elapsed := load(ctx, conn, method)
	slices.Sort(lat)
	rt.LoadErrors = errs
	rt.RPS = float64(int(float64(loadCalls) / elapsed.Seconds()))
	rt.P50Ms, rt.P90Ms, rt.P99Ms = pct(lat, 0.50), pct(lat, 0.90), pct(lat, 0.99)
	x.log(st, fmt.Sprintf("load %s: %d calls, %d workers, %.0f rps, p50 %.3f ms, p90 %.3f ms, p99 %.3f ms, %d errors",
		method, loadCalls, loadConcurrency, rt.RPS, rt.P50Ms, rt.P90Ms, rt.P99Ms, errs))

	if b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(cmd.Process.Pid)).Output(); err == nil {
		if kb, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			rt.RSSBytes = kb * 1024
			x.log(st, fmt.Sprintf("resident memory after load: %.1f MB", float64(rt.RSSBytes)/(1<<20)))
		}
	}

	t := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		stopped = true
		rt.ShutdownMs = float64(time.Since(t).Microseconds()) / 1000
		rt.CleanExit = err == nil
	case <-time.After(15 * time.Second):
		return errors.New("server did not stop within 15s of SIGTERM")
	}
	x.log(st, fmt.Sprintf("SIGTERM → exit in %.1f ms, clean exit: %v", rt.ShutdownMs, rt.CleanExit))

	switch {
	case model.Health && rt.HealthStatus != "SERVING":
		return fmt.Errorf("health status is %s, want SERVING", rt.HealthStatus)
	case errs > 0:
		return fmt.Errorf("%d of %d load calls failed", errs, loadCalls)
	case !rt.CleanExit:
		return errors.New("server exited with an error after SIGTERM")
	}
	x.summary(st, "%s store: ready in %.0f ms; p99 %.2f ms at %.0f rps; %.0f MB RSS; clean shutdown", storeKind, rt.StartupMs, rt.P99Ms, rt.RPS, float64(rt.RSSBytes)/(1<<20))
	return nil
}

// waitReady polls the TCP port (cheap, no client backoff), then the gRPC
// health service until it reports SERVING.
func waitReady(ctx context.Context, conn *grpc.ClientConn, addr string, health bool, exited chan error) error {
	deadline := time.Now().Add(10 * time.Second)
	exitedEarly := func() error {
		select {
		case err := <-exited:
			exited <- err // let the caller's cleanup see the exit too
			return fmt.Errorf("server exited during startup: %w", err)
		default:
			return nil
		}
	}
	for {
		if err := exitedEarly(); err != nil {
			return err
		}
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			return errors.New("server did not open its port within 10s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !health {
		return nil
	}
	hc := healthpb.NewHealthClient(conn)
	for time.Now().Before(deadline) {
		if err := exitedEarly(); err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		resp, err := hc.Check(cctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
		cancel()
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return errors.New("health service did not report SERVING within 10s")
}

func listServices(ctx context.Context, conn *grpc.ClientConn) ([]string, error) {
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.CloseSend() }()
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: "*"},
	}); err != nil {
		return nil, err
	}
	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		out = append(out, s.GetName())
	}
	slices.Sort(out)
	return out, nil
}

// load calls method with an empty request (valid for List RPCs and health)
// from several workers and returns per-call latencies in milliseconds.
func load(ctx context.Context, conn *grpc.ClientConn, method string) ([]float64, int, time.Duration) {
	var mu sync.Mutex
	lat := make([]float64, 0, loadCalls)
	errs := 0
	jobs := make(chan struct{}, loadCalls)
	for range loadCalls {
		jobs <- struct{}{}
	}
	close(jobs)
	start := time.Now()
	var wg sync.WaitGroup
	for range loadConcurrency {
		wg.Go(func() {
			local := make([]float64, 0, loadCalls/loadConcurrency+1)
			localErrs := 0
			for range jobs {
				t := time.Now()
				if err := conn.Invoke(ctx, method, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
					localErrs++
				}
				local = append(local, float64(time.Since(t).Microseconds())/1000)
			}
			mu.Lock()
			lat = append(lat, local...)
			errs += localErrs
			mu.Unlock()
		})
	}
	wg.Wait()
	return lat, errs, time.Since(start)
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
