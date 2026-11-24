package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/tillknuesting/fliessband/internal/deploy"
	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/pipeline"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// DeployService describes a run's service for the deployer.
func DeployService(run *pipeline.Run) (deploy.Service, error) {
	src, _ := run.Source()
	s, _, err := spec.Parse([]byte(src))
	if err != nil {
		return deploy.Service{}, err
	}
	m := gen.NewModel(s)
	return deploy.Service{Run: run.ID, Name: m.Name, FullName: m.FullServiceName, Port: m.Port, Health: m.Health,
		Postgres: m.Postgres, Dir: run.ServiceDir()}, nil
}

func (a *api) deployer(w http.ResponseWriter) *deploy.Deployer {
	if a.cfg.Deployer == nil {
		writeErr(w, http.StatusNotImplemented, "deployment is off")
	}
	return a.cfg.Deployer
}

// deployState lists the targets as their clusters report them, and recent deployments.
func (a *api) deployState(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Deployer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "targets": []any{}, "deployments": []any{}})
		return
	}
	deps := a.cfg.Deployer.List()
	if len(deps) > 100 {
		deps = deps[:100]
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "targets": a.cfg.Deployer.State(), "deployments": deps})
}

func (a *api) deployRun(w http.ResponseWriter, r *http.Request) {
	d := a.deployer(w)
	if d == nil {
		return
	}
	run := a.run(w, r)
	if run == nil {
		return
	}
	req := struct {
		Target string `json:"target"`
	}{}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Target == "" {
		writeErr(w, http.StatusBadRequest, `send {"target": "<name>"}`)
		return
	}
	if st := run.Summary().Status; st != pipeline.Passed {
		writeErr(w, http.StatusConflict, "only a run that passed the line can be deployed; this one is "+string(st))
		return
	}
	svc, err := DeployService(run)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	dep, err := d.Deploy(req.Target, svc)
	switch {
	case errors.Is(err, deploy.ErrNoTarget):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, deploy.ErrBusy):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.cfg.Log.Info("deployment started", "deployment", dep.ID, "run", run.ID, "target", req.Target, "service", svc.Name)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": dep.ID})
}

func (a *api) deployment(w http.ResponseWriter, r *http.Request) {
	d := a.deployer(w)
	if d == nil {
		return
	}
	dep := d.Get(r.PathValue("id"))
	if dep == nil {
		writeErr(w, http.StatusNotFound, "no deployment with id "+r.PathValue("id"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(dep.JSON())
}

func (a *api) removeService(w http.ResponseWriter, r *http.Request) {
	d := a.deployer(w)
	if d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	dep, err := d.Remove(ctx, r.PathValue("target"), r.PathValue("service"))
	switch {
	case errors.Is(err, deploy.ErrNoTarget):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, deploy.ErrBusy):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case dep == nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.cfg.Log.Info("service removed", "target", dep.Target, "service", dep.Service, "err", err)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(dep.JSON())
}
