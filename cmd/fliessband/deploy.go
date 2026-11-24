package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tillknuesting/fliessband/internal/deploy"
	"github.com/tillknuesting/fliessband/internal/pipeline"
	"github.com/tillknuesting/fliessband/internal/server"
)

// setupDeployer reads the deploy targets: FLIESSBAND_DEPLOY_TARGETS, or
// else the local clusters in the kubeconfig.
func setupDeployer(m *pipeline.Manager) (*deploy.Deployer, error) {
	targets, err := deploy.ParseTargets(os.Getenv("FLIESSBAND_DEPLOY_TARGETS"))
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		targets = deploy.Discover(context.Background(), deploy.BaseEnv())
	}
	return deploy.New(m.DataDir(), targets, m.DockerEnv)
}

func deployCmd(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	data := fs.String("data", envOr("FLIESSBAND_DATA", "data"), "directory for runs")
	target := fs.String("target", "", "deploy target (default: the only one)")
	list := fs.Bool("list", false, "list the targets and what runs on them")
	remove := fs.String("remove", "", "remove this service from the target")
	_ = fs.Parse(args)

	m, err := pipeline.NewManager(*data, toolsDir(), nil)
	if err != nil {
		return err
	}
	d, err := setupDeployer(m)
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *list {
		for _, t := range d.State() {
			state := "reachable, Kubernetes " + t.Server
			if !t.Reachable {
				state = t.Error
			}
			fmt.Printf("%s\t%s/%s\t%s\t%s\n", t.Name, t.Context, t.Namespace, t.Strategy, state)
			for _, w := range t.Workloads {
				fmt.Printf("  %s\trun %s\t%d/%d ready\n", w.Service, w.Run, w.Ready, w.Desired)
			}
		}
		if len(d.Targets()) == 0 {
			fmt.Println("no deploy targets: create a local cluster (k3d cluster create fliessband) or set FLIESSBAND_DEPLOY_TARGETS")
		}
		return nil
	}
	name, err := pickTarget(d, *target)
	if err != nil {
		return err
	}
	if *remove != "" {
		dep, err := d.Remove(ctx, name, *remove)
		if dep != nil {
			printLog(dep, 0)
		}
		if err == nil {
			fmt.Printf("removed %s from %s\n", *remove, name)
		}
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: fliessband deploy [-target NAME] RUN-ID")
	}
	run := m.Get(fs.Arg(0))
	if run == nil {
		return fmt.Errorf("no run with id %s", fs.Arg(0))
	}
	if st := run.Summary().Status; st != pipeline.Passed {
		return fmt.Errorf("run %s %s; only a run that passed the line can be deployed", run.ID, st)
	}
	svc, err := server.DeployService(run)
	if err != nil {
		return err
	}
	dep, err := d.Deploy(name, svc)
	if err != nil {
		return err
	}
	fmt.Printf("deploying %s (run %s) to %s\n", svc.Name, run.ID, name)
	seen := 0
	for {
		seen = printLog(dep, seen)
		var v struct {
			Status deploy.Status `json:"status"`
			Error  string        `json:"error"`
		}
		_ = json.Unmarshal(dep.JSON(), &v)
		if v.Status != deploy.Running {
			fmt.Println("status:", v.Status)
			if v.Status != deploy.Live {
				return fmt.Errorf("deployment %s: %s", v.Status, v.Error)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			d.Close()
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func pickTarget(d *deploy.Deployer, name string) (string, error) {
	ts := d.Targets()
	if name != "" {
		return name, nil
	}
	switch len(ts) {
	case 0:
		return "", errors.New("no deploy targets: create a local cluster (k3d cluster create fliessband) or set FLIESSBAND_DEPLOY_TARGETS")
	case 1:
		return ts[0].Name, nil
	}
	return "", errors.New("more than one deploy target: choose one with -target (see fliessband deploy -list)")
}

// printLog prints the deployment's log lines from index seen on.
func printLog(dep *deploy.Deployment, seen int) int {
	var v struct {
		Log []string `json:"log"`
	}
	_ = json.Unmarshal(dep.JSON(), &v)
	for _, l := range v.Log[min(seen, len(v.Log)):] {
		fmt.Println(l)
	}
	return len(v.Log)
}
