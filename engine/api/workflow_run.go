package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-gorp/gorp"
	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/cache"
	"github.com/ovh/cds/engine/api/feature"
	"github.com/ovh/cds/engine/api/objectstore"
	"github.com/ovh/cds/engine/api/observability"
	"github.com/ovh/cds/engine/api/permission"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/api/workflow"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

const (
	rangeMax     = 50
	defaultLimit = 10
)

func (api *API) searchWorkflowRun(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string, route, key, name string) error {
	// About pagination: [FR] http://blog.octo.com/designer-une-api-rest/#pagination
	var limit, offset int

	offsetS := r.FormValue("offset")
	var errAtoi error
	if offsetS != "" {
		offset, errAtoi = strconv.Atoi(offsetS)
		if errAtoi != nil {
			return sdk.ErrWrongRequest
		}
	}
	limitS := r.FormValue("limit")
	if limitS != "" {
		limit, errAtoi = strconv.Atoi(limitS)
		if errAtoi != nil {
			return sdk.ErrWrongRequest
		}
	}

	if offset < 0 {
		offset = 0
	}
	if limit == 0 {
		limit = defaultLimit
	}

	//Parse all form values
	mapFilters := map[string]string{}
	for k := range r.Form {
		if k != "offset" && k != "limit" && k != "workflow" {
			mapFilters[k] = r.FormValue(k)
		}
	}

	//Maximim range is set to 50
	w.Header().Add("Accept-Range", "run 50")
	runs, offset, limit, count, err := workflow.LoadRuns(api.mustDB(), key, name, offset, limit, mapFilters)
	if err != nil {
		return sdk.WrapError(err, "searchWorkflowRun> Unable to load workflow runs")
	}

	code := http.StatusOK

	//RFC5988: Link : <https://api.fakecompany.com/v1/orders?range=0-7>; rel="first", <https://api.fakecompany.com/v1/orders?range=40-47>; rel="prev", <https://api.fakecompany.com/v1/orders?range=56-64>; rel="next", <https://api.fakecompany.com/v1/orders?range=968-975>; rel="last"
	if len(runs) < count {
		baseLinkURL := api.Router.URL + route
		code = http.StatusPartialContent

		//First page
		firstLimit := limit - offset
		if firstLimit > count {
			firstLimit = count
		}
		firstLink := fmt.Sprintf(`<%s?offset=0&limit=%d>; rel="first"`, baseLinkURL, firstLimit)
		link := firstLink

		//Prev page
		if offset != 0 {
			prevOffset := offset - (limit - offset)
			prevLimit := offset
			if prevOffset < 0 {
				prevOffset = 0
			}
			prevLink := fmt.Sprintf(`<%s?offset=%d&limit=%d>; rel="prev"`, baseLinkURL, prevOffset, prevLimit)
			link = link + ", " + prevLink
		}

		//Next page
		if limit < count {
			nextOffset := limit
			nextLimit := limit + (limit - offset)

			if nextLimit >= count {
				nextLimit = count
			}

			nextLink := fmt.Sprintf(`<%s?offset=%d&limit=%d>; rel="next"`, baseLinkURL, nextOffset, nextLimit)
			link = link + ", " + nextLink
		}

		//Last page
		lastOffset := count - (limit - offset)
		if lastOffset < 0 {
			lastOffset = 0
		}
		lastLimit := count
		lastLink := fmt.Sprintf(`<%s?offset=%d&limit=%d>; rel="last"`, baseLinkURL, lastOffset, lastLimit)
		link = link + ", " + lastLink

		w.Header().Add("Link", link)
	}

	w.Header().Add("Content-Range", fmt.Sprintf("%d-%d/%d", offset, limit, count))

	for i := range runs {
		runs[i].Translate(r.Header.Get("Accept-Language"))
	}

	// Return empty array instead of nil
	if runs == nil {
		runs = []sdk.WorkflowRun{}
	}
	return service.WriteJSON(w, runs, code)
}

func (api *API) getWorkflowAllRunsHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["permProjectKey"]
		name := r.FormValue("workflow")
		route := api.Router.GetRoute("GET", api.getWorkflowAllRunsHandler, map[string]string{
			"permProjectKey": key,
		})
		return api.searchWorkflowRun(ctx, w, r, vars, route, key, name)
	}
}

func (api *API) getWorkflowRunsHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		route := api.Router.GetRoute("GET", api.getWorkflowRunsHandler, map[string]string{
			"key":          key,
			"workflowName": name,
		})
		return api.searchWorkflowRun(ctx, w, r, vars, route, key, name)
	}
}

// getWorkflowRunNumHandler returns the last run number for the given workflow
func (api *API) getWorkflowRunNumHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]

		num, err := workflow.LoadCurrentRunNum(api.mustDB(), key, name)
		if err != nil {
			return sdk.WrapError(err, "getWorkflowRunNumHandler> Cannot load current run num")
		}

		return service.WriteJSON(w, sdk.WorkflowRunNumber{Num: num}, http.StatusOK)
	}
}

// postWorkflowRunNumHandler updates the current run number for the given workflow
func (api *API) postWorkflowRunNumHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]

		m := struct {
			Num int64 `json:"num"`
		}{}

		if err := UnmarshalBody(r, &m); err != nil {
			return sdk.WrapError(err, "postWorkflowRunNumHandler>")
		}

		num, err := workflow.LoadCurrentRunNum(api.mustDB(), key, name)
		if err != nil {
			return sdk.WrapError(err, "postWorkflowRunNumHandler> Cannot load current run num")
		}

		if m.Num < num {
			return sdk.WrapError(sdk.ErrWrongRequest, "postWorkflowRunNumHandler> Cannot num must be > %d, got %d", num, m.Num)
		}

		proj, err := project.Load(api.mustDB(), api.Cache, key, getUser(ctx), project.LoadOptions.WithPlatforms)
		if err != nil {
			return sdk.WrapError(err, "postWorkflowRunNumHandler> unable to load projet")
		}

		options := workflow.LoadOptions{
			WithoutNode: true,
		}
		wf, errW := workflow.Load(ctx, api.mustDB(), api.Cache, proj, name, getUser(ctx), options)
		if errW != nil {
			return sdk.WrapError(errW, "postWorkflowRunNumHandler > Cannot load workflow")
		}

		var errDb error
		if num == 0 {
			errDb = workflow.InsertRunNum(api.mustDB(), wf, m.Num)
		} else {
			errDb = workflow.UpdateRunNum(api.mustDB(), wf, m.Num)
		}

		if errDb != nil {
			return sdk.WrapError(errDb, "postWorkflowRunNumHandler> ")
		}

		return service.WriteJSON(w, m, http.StatusOK)
	}
}

func (api *API) getLatestWorkflowRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		run, err := workflow.LoadLastRun(api.mustDB(), key, name, workflow.LoadRunOptions{WithArtifacts: true})
		if err != nil {
			return sdk.WrapError(err, "getLatestWorkflowRunHandler> Unable to load last workflow run")
		}
		run.Translate(r.Header.Get("Accept-Language"))
		return service.WriteJSON(w, run, http.StatusOK)
	}
}

func (api *API) resyncWorkflowRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}

		proj, err := project.Load(api.mustDB(), api.Cache, key, getUser(ctx), project.LoadOptions.WithPlatforms)
		if err != nil {
			return sdk.WrapError(err, "resyncWorkflowRunHandler> unable to load projet")
		}

		run, err := workflow.LoadRun(api.mustDB(), key, name, number, workflow.LoadRunOptions{})
		if err != nil {
			return sdk.WrapError(err, "resyncWorkflowRunHandler> Unable to load last workflow run [%s/%d]", name, number)
		}

		tx, errT := api.mustDB().Begin()
		if errT != nil {
			return sdk.WrapError(errT, "resyncWorkflowRunHandler> Cannot start transaction")
		}

		if err := workflow.Resync(tx, api.Cache, proj, run, getUser(ctx)); err != nil {
			return sdk.WrapError(err, "resyncWorkflowRunHandler> Cannot resync pipelines")
		}

		if err := tx.Commit(); err != nil {
			return sdk.WrapError(err, "resyncWorkflowRunHandler> Cannot commit transaction")
		}
		return service.WriteJSON(w, run, http.StatusOK)
	}
}

func (api *API) getWorkflowRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}
		run, err := workflow.LoadRun(api.mustDB(), key, name, number,
			workflow.LoadRunOptions{
				WithArtifacts:           true,
				WithLightTests:          true,
				DisableDetailledNodeRun: true,
			},
		)
		if err != nil {
			return sdk.WrapError(err, "getWorkflowRunHandler> Unable to load workflow %s run number %d", name, number)
		}
		run.Translate(r.Header.Get("Accept-Language"))
		return service.WriteJSON(w, run, http.StatusOK)
	}
}

func (api *API) stopWorkflowRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}

		run, errL := workflow.LoadRun(api.mustDB(), key, name, number, workflow.LoadRunOptions{})
		if errL != nil {
			return sdk.WrapError(errL, "stopWorkflowRunHandler> Unable to load last workflow run")
		}

		proj, errP := project.Load(api.mustDB(), api.Cache, key, getUser(ctx))
		if errP != nil {
			return sdk.WrapError(errP, "stopWorkflowRunHandler> Unable to load project")
		}

		report, err := stopWorkflowRun(ctx, api.mustDB, api.Cache, proj, run, getUser(ctx))
		if err != nil {
			return sdk.WrapError(err, "stopWorkflowRun> Unable to stop workflow")
		}

		workflowRuns, workflowNodeRuns := workflow.GetWorkflowRunEventData(report, proj.Key)
		go workflow.SendEvent(api.mustDB(), workflowRuns, workflowNodeRuns, proj.Key)

		return service.WriteJSON(w, run, http.StatusOK)
	}
}

func stopWorkflowRun(ctx context.Context, dbFunc func() *gorp.DbMap, store cache.Store, p *sdk.Project, run *sdk.WorkflowRun, u *sdk.User) (*workflow.ProcessorReport, error) {
	report := new(workflow.ProcessorReport)

	tx, errTx := dbFunc().Begin()
	if errTx != nil {
		return nil, sdk.WrapError(errTx, "stopWorkflowRunHandler> Unable to create transaction")
	}
	defer tx.Rollback() //nolint

	spwnMsg := sdk.SpawnMsg{ID: sdk.MsgWorkflowNodeStop.ID, Args: []interface{}{u.Username}}

	stopInfos := sdk.SpawnInfo{
		APITime:    time.Now(),
		RemoteTime: time.Now(),
		Message:    spwnMsg,
	}

	workflow.AddWorkflowRunInfo(run, false, spwnMsg)

	for _, wn := range run.WorkflowNodeRuns {
		for _, wnr := range wn {
			if wnr.SubNumber != run.LastSubNumber || (wnr.Status == sdk.StatusSuccess.String() ||
				wnr.Status == sdk.StatusFail.String() || wnr.Status == sdk.StatusSkipped.String()) {
				log.Debug("stopWorkflowRunHandler> cannot stop this workflow node run with current status %s", wnr.Status)
				continue
			}

			r1, errS := workflow.StopWorkflowNodeRun(ctx, dbFunc, store, p, wnr, stopInfos)
			if errS != nil {
				return nil, sdk.WrapError(errS, "stopWorkflowRunHandler> Unable to stop workflow node run %d", wnr.ID)
			}
			_, _ = report.Merge(r1, nil)
			wnr.Status = sdk.StatusStopped.String()
		}
	}

	run.LastExecution = time.Now()
	run.Status = sdk.StatusStopped.String()
	if errU := workflow.UpdateWorkflowRun(ctx, tx, run); errU != nil {
		return nil, sdk.WrapError(errU, "stopWorkflowRunHandler> Unable to update workflow run %d", run.ID)
	}
	report.Add(*run)

	if err := tx.Commit(); err != nil {
		return nil, sdk.WrapError(err, "stopWorkflowRunHandler> Cannot commit transaction")
	}

	return report, nil
}

func (api *API) getWorkflowNodeRunHistoryHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}
		nodeID, err := requestVarInt(r, "nodeID")
		if err != nil {
			return err
		}

		run, errR := workflow.LoadRun(api.mustDB(), key, name, number, workflow.LoadRunOptions{DisableDetailledNodeRun: true})
		if errR != nil {
			return sdk.WrapError(errR, "getWorkflowNodeRunHistoryHandler")
		}

		nodeRuns, ok := run.WorkflowNodeRuns[nodeID]
		if !ok {
			return sdk.WrapError(sdk.ErrWorkflowNodeNotFound, "getWorkflowNodeRunHistoryHandler")
		}
		return service.WriteJSON(w, nodeRuns, http.StatusOK)
	}
}

func (api *API) getWorkflowCommitsHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		nodeName := vars["nodeName"]
		branch := FormString(r, "branch")
		hash := FormString(r, "hash")
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}

		proj, errP := project.Load(api.mustDB(), api.Cache, key, getUser(ctx), project.LoadOptions.WithPlatforms)
		if errP != nil {
			return sdk.WrapError(errP, "getWorkflowCommitsHandler> Unable to load project %s", key)
		}

		wf, errW := workflow.Load(ctx, api.mustDB(), api.Cache, proj, name, getUser(ctx), workflow.LoadOptions{})
		if errW != nil {
			return sdk.WrapError(errW, "getWorkflowCommitsHandler> Unable to load workflow %s", name)
		}

		var errCtx error
		var nodeCtx *sdk.WorkflowNodeContext
		var wNode *sdk.WorkflowNode
		wfRun, errW := workflow.LoadRun(api.mustDB(), key, name, number, workflow.LoadRunOptions{DisableDetailledNodeRun: true})
		if errW == nil {
			wNode = wfRun.Workflow.GetNodeByName(nodeName)
		}

		if wNode == nil || errW != nil {
			log.Debug("getWorkflowCommitsHandler> node not found")
			nodeCtx, errCtx = workflow.LoadNodeContextByNodeName(api.mustDB(), api.Cache, proj, getUser(ctx), name, nodeName, workflow.LoadOptions{})
			if errCtx != nil {
				return sdk.WrapError(errCtx, "getWorkflowCommitsHandler> Unable to load workflow node context")
			}
		} else if wNode != nil {
			nodeCtx = wNode.Context
		} else {
			return sdk.WrapError(errW, "getWorkflowCommitsHandler> Unable to load workflow node run")
		}

		if nodeCtx == nil || nodeCtx.Application == nil {
			return service.WriteJSON(w, []sdk.VCSCommit{}, http.StatusOK)
		}

		if wfRun == nil {
			wfRun = &sdk.WorkflowRun{Number: number}
		}
		wfNodeRun := &sdk.WorkflowNodeRun{}
		if branch != "" {
			wfNodeRun.VCSBranch = branch
		}
		if hash != "" {
			wfNodeRun.VCSHash = hash
		} else if wNode != nil && errW == nil && wNode.ID != wfRun.Workflow.Root.ID {
			// Find hash and branch of ancestor node run
			nodeIDsAncestors := wNode.Ancestors(&wfRun.Workflow, false)
			for _, ancestorID := range nodeIDsAncestors {
				if wfRun.WorkflowNodeRuns[ancestorID][0].VCSRepository == nodeCtx.Application.RepositoryFullname {
					wfNodeRun.VCSHash = wfRun.WorkflowNodeRuns[ancestorID][0].VCSHash
					wfNodeRun.VCSBranch = wfRun.WorkflowNodeRuns[ancestorID][0].VCSBranch
					break
				}
			}
		}

		log.Debug("getWorkflowCommitsHandler> VCSHash: %s VCSBranch: %s", wfNodeRun.VCSHash, wfNodeRun.VCSBranch)
		commits, _, errC := workflow.GetNodeRunBuildCommits(ctx, api.mustDB(), api.Cache, proj, wf, nodeName, wfRun.Number, wfNodeRun, nodeCtx.Application, nodeCtx.Environment)
		if errC != nil {
			return sdk.WrapError(errC, "getWorkflowCommitsHandler> Unable to load commits: %v", errC)
		}

		return service.WriteJSON(w, commits, http.StatusOK)
	}
}

func (api *API) stopWorkflowNodeRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}
		id, err := requestVarInt(r, "nodeRunID")
		if err != nil {
			return err
		}

		p, errP := project.Load(api.mustDB(), api.Cache, key, getUser(ctx), project.LoadOptions.WithVariables)
		if errP != nil {
			return sdk.WrapError(errP, "stopWorkflowNodeRunHandler> Cannot load project")
		}

		// Load node run
		nodeRun, err := workflow.LoadNodeRun(api.mustDB(), key, name, number, id, workflow.LoadRunOptions{})
		if err != nil {
			return sdk.WrapError(err, "stopWorkflowNodeRunHandler> Unable to load last workflow run")
		}

		report, err := stopWorkflowNodeRun(ctx, api.mustDB, api.Cache, p, nodeRun, name, getUser(ctx))
		if err != nil {
			return sdk.WrapError(err, "stopWorkflowNodeRunHandler> Unable to stop workflow run")
		}

		workflowRuns, workflowNodeRuns := workflow.GetWorkflowRunEventData(report, p.Key)
		go workflow.SendEvent(api.mustDB(), workflowRuns, workflowNodeRuns, p.Key)

		return service.WriteJSON(w, nodeRun, http.StatusOK)
	}
}

func stopWorkflowNodeRun(ctx context.Context, dbFunc func() *gorp.DbMap, store cache.Store, p *sdk.Project, nodeRun *sdk.WorkflowNodeRun, workflowName string, u *sdk.User) (*workflow.ProcessorReport, error) {
	tx, errTx := dbFunc().Begin()
	if errTx != nil {
		return nil, sdk.WrapError(errTx, "stopWorkflowNodeRunHandler> Unable to create transaction")
	}
	defer tx.Rollback()

	stopInfos := sdk.SpawnInfo{
		APITime:    time.Now(),
		RemoteTime: time.Now(),
		Message:    sdk.SpawnMsg{ID: sdk.MsgWorkflowNodeStop.ID, Args: []interface{}{u.Username}},
	}
	report, errS := workflow.StopWorkflowNodeRun(ctx, dbFunc, store, p, *nodeRun, stopInfos)
	if errS != nil {
		return nil, sdk.WrapError(errS, "stopWorkflowNodeRunHandler> Unable to stop workflow node run")
	}

	wr, errLw := workflow.LoadRun(tx, p.Key, workflowName, nodeRun.Number, workflow.LoadRunOptions{})
	if errLw != nil {
		return nil, sdk.WrapError(errLw, "stopWorkflowNodeRunHandler> Unable to load workflow run %s", workflowName)
	}

	r1, errR := workflow.ResyncWorkflowRunStatus(tx, wr)
	if errR != nil {
		return nil, sdk.WrapError(errR, "stopWorkflowNodeRunHandler> Unable to resync workflow run status")
	}

	_, _ = report.Merge(r1, nil)

	if errC := tx.Commit(); errC != nil {
		return nil, sdk.WrapError(errC, "stopWorkflowNodeRunHandler> Unable to commit")
	}

	return report, nil
}

func (api *API) getWorkflowNodeRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}
		id, err := requestVarInt(r, "nodeRunID")
		if err != nil {
			return err
		}
		run, err := workflow.LoadNodeRun(api.mustDB(), key, name, number, id, workflow.LoadRunOptions{WithTests: true, WithArtifacts: true, WithCoverage: true, WithVulnerabilities: true})
		if err != nil {
			return sdk.WrapError(err, "getWorkflowNodeRunHandler> Unable to load last workflow run")
		}

		run.Translate(r.Header.Get("Accept-Language"))
		return service.WriteJSON(w, run, http.StatusOK)
	}
}

func (api *API) postWorkflowRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		u := getUser(ctx)

		observability.Current(ctx, observability.Tag(observability.TagWorkflow, name))
		observability.Record(ctx, api.Stats.WorkflowRuns, 1)

		_, next := observability.Span(ctx, "project.Load")
		p, errP := project.Load(api.mustDB(), api.Cache, key, u,
			project.LoadOptions.WithVariables,
			project.LoadOptions.WithFeatures,
			project.LoadOptions.WithPlatforms,
			project.LoadOptions.WithApplicationVariables,
			project.LoadOptions.WithApplicationWithDeploymentStrategies,
		)
		next()
		if errP != nil {
			return sdk.WrapError(errP, "postWorkflowRunHandler> Cannot load project")
		}

		opts := &sdk.WorkflowRunPostHandlerOption{}
		if err := UnmarshalBody(r, opts); err != nil {
			return err
		}

		var lastRun *sdk.WorkflowRun
		var asCodeInfosMsg []sdk.Message
		if opts.Number != nil {
			var errlr error
			_, next := observability.Span(ctx, "workflow.LoadRun")
			lastRun, errlr = workflow.LoadRun(api.mustDB(), key, name, *opts.Number, workflow.LoadRunOptions{})
			next()
			if errlr != nil {
				return sdk.WrapError(errlr, "postWorkflowRunHandler> Unable to load workflow run")
			}
		}

		var wf *sdk.Workflow
		if lastRun != nil {
			wf = &lastRun.Workflow
			// Check workflow name in case of rename
			if wf.Name != name {
				wf.Name = name
			}
		} else {
			// Test workflow as code or not
			options := workflow.LoadOptions{
				OnlyRootNode: true,
				DeepPipeline: false,
				Base64Keys:   true,
			}
			var errW error
			wf, errW = workflow.Load(ctx, api.mustDB(), api.Cache, p, name, u, options)
			if errW != nil {
				return sdk.WrapError(errW, "postWorkflowRunHandler> Unable to load workflow %s", name)
			}

			enabled, has := p.Features[feature.FeatWorkflowAsCode]
			if wf.FromRepository != "" {
				if has && !enabled {
					return sdk.WrapError(sdk.ErrForbidden, "postWorkflowRunHandler> %s not allowed for project %s", feature.FeatWorkflowAsCode, p.Key)
				}
				proj, errp := project.Load(api.mustDB(), api.Cache, key, u,
					project.LoadOptions.WithGroups,
					project.LoadOptions.WithApplicationVariables,
					project.LoadOptions.WithApplicationWithDeploymentStrategies,
					project.LoadOptions.WithEnvironments,
					project.LoadOptions.WithPipelines,
					project.LoadOptions.WithClearKeys,
					project.LoadOptions.WithClearPlatforms,
				)

				if errp != nil {
					return sdk.WrapError(errp, "postWorkflowRunHandler> Cannot load project %s", key)
				}
				// Get workflow from repository
				var errCreate error
				asCodeInfosMsg, errCreate = workflow.CreateFromRepository(ctx, api.mustDB(), api.Cache, proj, wf, *opts, u, project.DecryptWithBuiltinKey)
				if errCreate != nil {
					return sdk.WrapError(errCreate, "postWorkflowRunHandler> Unable to get workflow from repository")
				}
			} else {
				var errl error
				options := workflow.LoadOptions{
					DeepPipeline: true,
					Base64Keys:   true,
				}
				wf, errl = workflow.Load(ctx, api.mustDB(), api.Cache, p, name, u, options)
				if errl != nil {
					return sdk.WrapError(errl, "postWorkflowRunHandler> Unable to load workflow %s/%s", key, name)
				}
			}
		}

		report, errS := startWorkflowRun(ctx, api.mustDB(), api.Cache, p, wf, lastRun, opts, u, asCodeInfosMsg)
		if errS != nil {
			return sdk.WrapError(errS, "postWorkflowRunHandler> Unable to start workflow %s/%s", key, name)
		}
		workflowRuns, workflowNodeRuns := workflow.GetWorkflowRunEventData(report, p.Key)
		workflow.ResyncNodeRunsWithCommits(ctx, api.mustDB(), api.Cache, p, workflowNodeRuns)
		go workflow.SendEvent(api.mustDB(), workflowRuns, workflowNodeRuns, p.Key)

		// Purge workflow run
		sdk.GoRoutine(
			"workflow.PurgeWorkflowRun",
			func() {
				if err := workflow.PurgeWorkflowRun(api.mustDB(), *wf); err != nil {
					log.Error("workflow.PurgeWorkflowRun> error %v", err)
				}
			})

		var wr *sdk.WorkflowRun
		if len(workflowRuns) > 0 {
			wr = &workflowRuns[0]
			wr.Translate(r.Header.Get("Accept-Language"))
		}
		return service.WriteJSON(w, wr, http.StatusAccepted)
	}
}

func startWorkflowRun(ctx context.Context, db *gorp.DbMap, store cache.Store, p *sdk.Project, wf *sdk.Workflow, lastRun *sdk.WorkflowRun, opts *sdk.WorkflowRunPostHandlerOption, u *sdk.User, asCodeInfos []sdk.Message) (*workflow.ProcessorReport, error) {
	ctx, end := observability.Span(ctx, "api.startWorkflowRun")
	defer end()

	report := new(workflow.ProcessorReport)

	tx, errb := db.Begin()
	if errb != nil {
		return nil, sdk.WrapError(errb, "startWorkflowRun> Cannot start transaction")
	}
	defer tx.Rollback()

	//Run from hook
	if opts.Hook != nil {
		_, r1, err := workflow.RunFromHook(ctx, db, tx, store, p, wf, opts.Hook, asCodeInfos)
		if err != nil {
			return nil, sdk.WrapError(err, "startWorkflowRun> Unable to run workflow from hook")
		}

		//Commit and return success
		if err := tx.Commit(); err != nil {
			return nil, sdk.WrapError(err, "startWorkflowRun> Unable to commit transaction")
		}

		return report.Merge(r1, nil)
	}

	//Default manual run
	if opts.Manual == nil {
		opts.Manual = &sdk.WorkflowNodeRunManual{}
	}
	opts.Manual.User = *u
	//Copy the user but empty groups and permissions
	opts.Manual.User.Groups = nil
	//Clean all permissions except for environments
	opts.Manual.User.Permissions = sdk.UserPermissions{
		EnvironmentsPerm: opts.Manual.User.Permissions.EnvironmentsPerm,
	}

	//Load the node from which we launch the workflow run
	fromNodes := []*sdk.WorkflowNode{}
	if len(opts.FromNodeIDs) > 0 {
		for _, fromNodeID := range opts.FromNodeIDs {
			fromNode := lastRun.Workflow.GetNode(fromNodeID)
			if fromNode == nil {
				return nil, sdk.WrapError(sdk.ErrWorkflowNodeNotFound, "startWorkflowRun> Payload: Unable to get node %d", fromNodeID)
			}
			fromNodes = append(fromNodes, fromNode)
		}
	} else {
		fromNodes = append(fromNodes, wf.Root)
	}

	//Run all the node asynchronously in a goroutines
	var wg = &sync.WaitGroup{}
	wg.Add(len(fromNodes))
	for i := 0; i < len(fromNodes); i++ {
		optsCopy := sdk.WorkflowRunPostHandlerOption{
			FromNodeIDs: opts.FromNodeIDs,
			Number:      opts.Number,
		}
		if opts.Manual != nil {
			optsCopy.Manual = &sdk.WorkflowNodeRunManual{
				PipelineParameters: opts.Manual.PipelineParameters,
				User:               opts.Manual.User,
				Payload:            opts.Manual.Payload,
			}
		}
		if opts.Hook != nil {
			optsCopy.Hook = &sdk.WorkflowNodeRunHookEvent{
				Payload:              opts.Hook.Payload,
				WorkflowNodeHookUUID: opts.Hook.WorkflowNodeHookUUID,
			}
		}
		go func(fromNode *sdk.WorkflowNode) {
			r1, err := runFromNode(ctx, db, store, optsCopy, p, wf, lastRun, u, fromNode)
			if err != nil {
				log.Error("error: %v", err)
				report.Add(err)
			}
			//since report is mutable and is a pointer and in this case we can't have any error, we can skip returned values
			_, _ = report.Merge(r1, nil)
			wg.Done()
		}(fromNodes[i])
	}

	wg.Wait()

	if report.Errors() != nil {
		//Just return the first error
		return nil, report.Errors()[0]
	}

	if lastRun == nil {
		_, r1, errmr := workflow.ManualRun(ctx, tx, store, p, wf, opts.Manual, asCodeInfos)
		if errmr != nil {
			return nil, sdk.WrapError(errmr, "startWorkflowRun> Unable to run workflow")
		}
		_, _ = report.Merge(r1, nil)
	}

	//Commit and return success
	if err := tx.Commit(); err != nil {
		return nil, sdk.WrapError(err, "startWorkflowRun> Unable to commit transaction")
	}

	return report, nil

}

func runFromNode(ctx context.Context, db *gorp.DbMap, store cache.Store, opts sdk.WorkflowRunPostHandlerOption, p *sdk.Project, wf *sdk.Workflow, lastRun *sdk.WorkflowRun, u *sdk.User, fromNode *sdk.WorkflowNode) (*workflow.ProcessorReport, error) {
	var end func()
	ctx, end = observability.Span(ctx, "runFromNode")
	defer end()

	tx, errb := db.Begin()
	if errb != nil {
		return nil, errb
	}

	defer tx.Rollback() // nolint

	report := new(workflow.ProcessorReport)

	// Check Env Permission
	if fromNode.Context.Environment != nil {
		if !permission.AccessToEnvironment(p.Key, fromNode.Context.Environment.Name, u, permission.PermissionReadExecute) {
			return nil, sdk.WrapError(sdk.ErrNoEnvExecution, "runFromNode> Not enough right to run on environment %s", fromNode.Context.Environment.Name)
		}
	}

	//If payload is not set, keep the default payload
	if opts.Manual.Payload == interface{}(nil) {
		opts.Manual.Payload = fromNode.Context.DefaultPayload
	}

	//If PipelineParameters are not set, keep the default PipelineParameters
	if len(opts.Manual.PipelineParameters) == 0 {
		opts.Manual.PipelineParameters = fromNode.Context.DefaultPipelineParameters
	}

	//Manual run
	if lastRun != nil {
		_, r1, errmr := workflow.ManualRunFromNode(ctx, tx, store, p, wf, lastRun.Number, opts.Manual, fromNode.ID)
		if errmr != nil {
			return nil, sdk.WrapError(errmr, "runFromNode> Unable to run workflow from node")
		}
		//since report is mutable and is a pointer and in this case we can't have any error, we can skip returned values
		_, _ = report.Merge(r1, nil)
	}

	if err := tx.Commit(); err != nil {
		return nil, sdk.WrapError(err, "runFromNode> Unable to commit transaction")

	}
	return report, nil
}

func (api *API) downloadworkflowArtifactDirectHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		hash := vars["hash"]

		art, err := workflow.LoadWorkfowArtifactByHash(api.mustDB(), hash)
		if err != nil {
			return sdk.WrapError(err, "downloadworkflowArtifactDirectHandler> Could not load artifact with hash %s", hash)
		}

		w.Header().Add("Content-Type", "application/octet-stream")
		w.Header().Add("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", art.Name))

		f, err := objectstore.Fetch(art)
		if err != nil {
			return sdk.WrapError(err, "downloadArtifactDirectHandler> Cannot fetch artifact")
		}

		if _, err := io.Copy(w, f); err != nil {
			_ = f.Close()
			return sdk.WrapError(err, "downloadPluginHandler> Cannot stream artifact")
		}

		if err := f.Close(); err != nil {
			return sdk.WrapError(err, "downloadPluginHandler> Cannot close artifact")
		}
		return nil
	}
}

func (api *API) getDownloadArtifactHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]

		id, errI := requestVarInt(r, "artifactId")
		if errI != nil {
			return sdk.WrapError(sdk.ErrInvalidID, "getDownloadArtifactHandler> Invalid node job run ID")
		}

		proj, err := project.Load(api.mustDB(), api.Cache, key, getUser(ctx), project.LoadOptions.WithPlatforms)
		if err != nil {
			return sdk.WrapError(err, "getDownloadArtifactHandler> unable to load projet")
		}

		options := workflow.LoadOptions{
			WithoutNode: true,
		}
		work, errW := workflow.Load(ctx, api.mustDB(), api.Cache, proj, name, getUser(ctx), options)
		if errW != nil {
			return sdk.WrapError(errW, "getDownloadArtifactHandler> Cannot load workflow")
		}

		art, errA := workflow.LoadArtifactByIDs(api.mustDB(), work.ID, id)
		if errA != nil {
			return sdk.WrapError(errA, "getDownloadArtifactHandler> Cannot load artifacts")
		}

		w.Header().Add("Content-Type", "application/octet-stream")
		w.Header().Add("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", art.Name))

		f, err := objectstore.Fetch(art)
		if err != nil {
			_ = f.Close()
			return sdk.WrapError(err, "getDownloadArtifactHandler> Cannot fetch artifact")
		}

		if _, err := io.Copy(w, f); err != nil {
			_ = f.Close()
			return sdk.WrapError(err, "getDownloadArtifactHandler> Cannot stream artifact")
		}

		if err := f.Close(); err != nil {
			return sdk.WrapError(err, "getDownloadArtifactHandler> Cannot close artifact")
		}
		return nil
	}
}

func (api *API) getWorkflowRunArtifactsHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]

		number, errNu := requestVarInt(r, "number")
		if errNu != nil {
			return sdk.WrapError(errNu, "getWorkflowJobArtifactsHandler> Invalid node job run ID")
		}

		wr, errW := workflow.LoadRun(api.mustDB(), key, name, number, workflow.LoadRunOptions{WithArtifacts: true})
		if errW != nil {
			return errW
		}

		arts := []sdk.WorkflowNodeRunArtifact{}
		for _, runs := range wr.WorkflowNodeRuns {
			if len(runs) == 0 {
				continue
			}

			sort.Slice(runs, func(i, j int) bool {
				return runs[i].SubNumber > runs[j].SubNumber
			})

			wg := &sync.WaitGroup{}
			for i := range runs[0].Artifacts {
				wg.Add(1)
				go func(a *sdk.WorkflowNodeRunArtifact) {
					defer wg.Done()
					url, _ := objectstore.FetchTempURL(a)
					if url != "" {
						a.TempURL = url
					}
				}(&runs[0].Artifacts[i])
			}
			wg.Wait()
			arts = append(arts, runs[0].Artifacts...)
		}

		return service.WriteJSON(w, arts, http.StatusOK)
	}
}

func (api *API) getWorkflowNodeRunJobServiceLogsHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		runJobID, errJ := requestVarInt(r, "runJobId")
		if errJ != nil {
			return sdk.WrapError(errJ, "getWorkflowNodeRunJobServiceLogsHandler> runJobId: invalid number")
		}
		db := api.mustDB()

		logsServices, err := workflow.LoadServicesLogsByJob(db, runJobID)
		if err != nil {
			return sdk.WrapError(err, "getWorkflowNodeRunJobServiceLogsHandler> cannot load service logs for node run job id %d", runJobID)
		}

		return service.WriteJSON(w, logsServices, http.StatusOK)
	}
}

func (api *API) getWorkflowNodeRunJobStepHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		projectKey := vars["key"]
		workflowName := vars["permWorkflowName"]
		number, errN := requestVarInt(r, "number")
		if errN != nil {
			return sdk.WrapError(errN, "getWorkflowNodeRunJobBuildLogsHandler> Number: invalid number")
		}
		nodeRunID, errNI := requestVarInt(r, "nodeRunID")
		if errNI != nil {
			return sdk.WrapError(errNI, "getWorkflowNodeRunJobBuildLogsHandler> id: invalid number")
		}
		runJobID, errJ := requestVarInt(r, "runJobId")
		if errJ != nil {
			return sdk.WrapError(errJ, "getWorkflowNodeRunJobBuildLogsHandler> runJobId: invalid number")
		}
		stepOrder, errS := requestVarInt(r, "stepOrder")
		if errS != nil {
			return sdk.WrapError(errS, "getWorkflowNodeRunJobBuildLogsHandler> stepOrder: invalid number")
		}

		// Check nodeRunID is link to workflow
		nodeRun, errNR := workflow.LoadNodeRun(api.mustDB(), projectKey, workflowName, number, nodeRunID, workflow.LoadRunOptions{DisableDetailledNodeRun: true})
		if errNR != nil {
			return sdk.WrapError(errNR, "getWorkflowNodeRunJobBuildLogsHandler> Cannot find nodeRun %d/%d for workflow %s in project %s", nodeRunID, number, workflowName, projectKey)
		}

		var stepStatus string
		// Find job/step in nodeRun
	stageLoop:
		for _, s := range nodeRun.Stages {
			for _, rj := range s.RunJobs {
				if rj.ID != runJobID {
					continue
				}
				ss := rj.Job.StepStatus
				for _, sss := range ss {
					if int64(sss.StepOrder) == stepOrder {
						stepStatus = sss.Status
						break
					}
				}
				break stageLoop
			}
		}

		if stepStatus == "" {
			return sdk.WrapError(sdk.ErrStepNotFound, "getWorkflowNodeRunJobStepHandler> Cannot find step %d on job %d in nodeRun %d/%d for workflow %s in project %s",
				stepOrder, runJobID, nodeRunID, number, workflowName, projectKey)
		}

		logs, errL := workflow.LoadStepLogs(api.mustDB(), runJobID, stepOrder)
		if errL != nil {
			return sdk.WrapError(errL, "getWorkflowNodeRunJobStepHandler> Cannot load log for runJob %d on step %d", runJobID, stepOrder)
		}

		ls := &sdk.Log{}
		if logs != nil {
			ls = logs
		}
		result := &sdk.BuildState{
			Status:   sdk.StatusFromString(stepStatus),
			StepLogs: *ls,
		}

		return service.WriteJSON(w, result, http.StatusOK)
	}
}

func (api *API) getWorkflowRunTagsHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		projectKey := vars["key"]
		workflowName := vars["permWorkflowName"]

		res, err := workflow.GetTagsAndValue(api.mustDB(), projectKey, workflowName)
		if err != nil {
			return sdk.WrapError(err, "getWorkflowRunTagsHandler> Error")
		}

		return service.WriteJSON(w, res, http.StatusOK)
	}
}

func (api *API) postResyncVCSWorkflowRunHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		db := api.mustDB()
		vars := mux.Vars(r)
		key := vars["key"]
		name := vars["permWorkflowName"]
		number, err := requestVarInt(r, "number")
		if err != nil {
			return err
		}

		proj, errP := project.Load(db, api.Cache, key, getUser(ctx), project.LoadOptions.WithVariables)
		if errP != nil {
			return sdk.WrapError(errP, "postResyncVCSWorkflowRunHandler> Cannot load project")
		}

		wfr, errW := workflow.LoadRun(db, key, name, number, workflow.LoadRunOptions{DisableDetailledNodeRun: true})
		if errW != nil {
			return sdk.WrapError(errW, "postResyncVCSWorkflowRunHandler> Cannot load workflow run")
		}

		if err := workflow.ResyncCommitStatus(ctx, db, api.Cache, proj, wfr); err != nil {
			return sdk.WrapError(err, "postResyncVCSWorkflowRunHandler> Cannot resync workflow run commit status")
		}

		return nil
	}
}
