package workflow

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fatih/structs"
	"github.com/go-gorp/gorp"

	"github.com/ovh/cds/engine/api/cache"
	"github.com/ovh/cds/engine/api/event"
	"github.com/ovh/cds/engine/api/observability"
	"github.com/ovh/cds/engine/api/repositoriesmanager"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

// GetWorkflowRunEventData read channel to get elements to push
// TODO: refactor this useless function
func GetWorkflowRunEventData(report *ProcessorReport, projectKey string) ([]sdk.WorkflowRun, []sdk.WorkflowNodeRun) {
	return report.workflows, report.nodes
}

// SendEvent Send event on workflow run
func SendEvent(db gorp.SqlExecutor, wrs []sdk.WorkflowRun, wnrs []sdk.WorkflowNodeRun, key string) {
	for _, wr := range wrs {
		event.PublishWorkflowRun(wr, key)
	}
	for _, wnr := range wnrs {
		wr, errWR := LoadRunByID(db, wnr.WorkflowRunID, LoadRunOptions{
			WithLightTests: true,
		})
		if errWR != nil {
			log.Warning("SendEvent.workflow> Cannot load workflow run %d: %s", wnr.WorkflowRunID, errWR)
			continue
		}

		var previousNodeRun sdk.WorkflowNodeRun
		if wnr.SubNumber > 0 {
			previousNodeRun = wnr
		} else {
			// Load previous run on current node
			node := wr.Workflow.GetNode(wnr.WorkflowNodeID)
			if node != nil {
				var errN error
				previousNodeRun, errN = PreviousNodeRun(db, wnr, *node, wr.WorkflowID)
				if errN != nil {
					log.Warning("SendEvent.workflow> Cannot load previous node run: %s", errN)
				}
			} else {
				log.Warning("SendEvent.workflow > Unable to find node %d in workflow", wnr.WorkflowNodeID)
			}
		}

		event.PublishWorkflowNodeRun(db, wnr, wr.Workflow, &previousNodeRun)
	}
}

// ResyncCommitStatus resync commit status for a workflow run
func ResyncCommitStatus(ctx context.Context, db gorp.SqlExecutor, store cache.Store, proj *sdk.Project, wr *sdk.WorkflowRun) error {
	_, end := observability.Span(ctx, "workflow.resyncCommitStatus",
		observability.Tag(observability.TagWorkflow, wr.Workflow.Name),
		observability.Tag(observability.TagWorkflowRun, wr.Number),
	)
	defer end()

	for nodeID, nodeRuns := range wr.WorkflowNodeRuns {
		sort.Slice(nodeRuns, func(i, j int) bool {
			return nodeRuns[i].SubNumber >= nodeRuns[j].SubNumber
		})

		nodeRun := nodeRuns[0]
		if !sdk.StatusIsTerminated(nodeRun.Status) {
			continue
		}

		node := wr.Workflow.GetNode(nodeID)
		if !node.IsLinkedToRepo() {
			return nil
		}

		vcsServer := repositoriesmanager.GetProjectVCSServer(proj, node.Context.Application.VCSServer)
		if vcsServer == nil {
			return nil
		}

		details := fmt.Sprintf("on project:%s workflow:%s node:%s num:%d sub:%d vcs:%s", proj.Name, wr.Workflow.Name, nodeRun.WorkflowNodeName, nodeRun.Number, nodeRun.SubNumber, vcsServer.Name)

		//Get the RepositoriesManager Client
		client, errClient := repositoriesmanager.AuthorizedClient(ctx, db, store, vcsServer)
		if errClient != nil {
			return sdk.WrapError(errClient, "resyncCommitStatus> Cannot get client %s", details)
		}

		ref := nodeRun.VCSHash
		if nodeRun.VCSTag != "" {
			ref = nodeRun.VCSTag
		}
		statuses, errStatuses := client.ListStatuses(ctx, node.Context.Application.RepositoryFullname, ref)
		if errStatuses != nil {
			return sdk.WrapError(errStatuses, "resyncCommitStatus> Cannot get statuses %s", details)
		}

		var statusFound *sdk.VCSCommitStatus
		expected := sdk.VCSCommitStatusDescription(proj.Key, wr.Workflow.Name, sdk.EventRunWorkflowNode{
			NodeName: node.Name,
		})

		for i, status := range statuses {
			if status.Decription == expected {
				statusFound = &statuses[i]
				break
			}
		}

		if statusFound == nil || statusFound.State == "" {
			if err := sendVCSEventStatus(ctx, db, store, proj, wr, &nodeRun); err != nil {
				log.Error("resyncCommitStatus> Error sending status %s err: %v", details, err)
			}
			continue
		}

		if statusFound.State == sdk.StatusBuilding.String() {
			if err := sendVCSEventStatus(ctx, db, store, proj, wr, &nodeRun); err != nil {
				log.Error("resyncCommitStatus> Error sending status %s err: %v", details, err)
			}
			continue
		}

		switch statusFound.State {
		case sdk.StatusBuilding.String():
			if err := sendVCSEventStatus(ctx, db, store, proj, wr, &nodeRun); err != nil {
				log.Error("resyncCommitStatus> Error sending status %s %s err:%v", statusFound.State, details, err)
			}
			continue

		case sdk.StatusSuccess.String():
			switch nodeRun.Status {
			case sdk.StatusSuccess.String():
				continue
			default:
				if err := sendVCSEventStatus(ctx, db, store, proj, wr, &nodeRun); err != nil {
					log.Error("resyncCommitStatus> Error sending status %s %s err:%v", statusFound.State, details, err)
				}
				continue
			}

		case sdk.StatusFail.String():
			switch nodeRun.Status {
			case sdk.StatusFail.String():
				continue
			default:
				if err := sendVCSEventStatus(ctx, db, store, proj, wr, &nodeRun); err != nil {
					log.Error("resyncCommitStatus> Error sending status %s %s err:%v", statusFound.State, details, err)
				}
				continue
			}

		case sdk.StatusSkipped.String():
			switch nodeRun.Status {
			case sdk.StatusDisabled.String(), sdk.StatusNeverBuilt.String(), sdk.StatusSkipped.String():
				continue
			default:
				if err := sendVCSEventStatus(ctx, db, store, proj, wr, &nodeRun); err != nil {
					log.Error("resyncCommitStatus> Error sending status %s %s err:%v", statusFound.State, details, err)
				}
				continue
			}
		}
	}
	return nil
}

// sendVCSEventStatus send status
func sendVCSEventStatus(ctx context.Context, db gorp.SqlExecutor, store cache.Store, proj *sdk.Project, wr *sdk.WorkflowRun, nodeRun *sdk.WorkflowNodeRun) error {
	log.Debug("Send status for node run %d", nodeRun.ID)

	node := wr.Workflow.GetNode(nodeRun.WorkflowNodeID)
	if !node.IsLinkedToRepo() {
		return nil
	}

	vcsServer := repositoriesmanager.GetProjectVCSServer(proj, node.Context.Application.VCSServer)
	if vcsServer == nil {
		return nil
	}
	//Get the RepositoriesManager Client
	client, errClient := repositoriesmanager.AuthorizedClient(ctx, db, store, vcsServer)
	if errClient != nil {
		return sdk.WrapError(errClient, "sendVCSEventStatus> Cannot get client")
	}

	var eventWNR = sdk.EventRunWorkflowNode{
		ID:             nodeRun.ID,
		Number:         nodeRun.Number,
		SubNumber:      nodeRun.SubNumber,
		Status:         nodeRun.Status,
		Start:          nodeRun.Start.Unix(),
		Done:           nodeRun.Done.Unix(),
		Manual:         nodeRun.Manual,
		HookEvent:      nodeRun.HookEvent,
		Payload:        nodeRun.Payload,
		SourceNodeRuns: nodeRun.SourceNodeRuns,
		Hash:           nodeRun.VCSHash,
		Tag:            nodeRun.VCSTag,
		BranchName:     nodeRun.VCSBranch,
		NodeID:         nodeRun.WorkflowNodeID,
		RunID:          nodeRun.WorkflowRunID,
		StagesSummary:  make([]sdk.StageSummary, len(nodeRun.Stages)),
		NodeName:       node.Name,
	}

	for i := range nodeRun.Stages {
		eventWNR.StagesSummary[i] = nodeRun.Stages[i].ToSummary()
	}

	var pipName, appName, envName string

	pipName = node.PipelineName
	appName = node.Context.Application.Name
	eventWNR.RepositoryManagerName = node.Context.Application.VCSServer
	eventWNR.RepositoryFullName = node.Context.Application.RepositoryFullname

	if node.Context.Environment != nil {
		envName = node.Context.Environment.Name
	}

	evt := sdk.Event{
		EventType:       fmt.Sprintf("%T", eventWNR),
		Payload:         structs.Map(eventWNR),
		Timestamp:       time.Now(),
		ProjectKey:      proj.Key,
		WorkflowName:    wr.Workflow.Name,
		PipelineName:    pipName,
		ApplicationName: appName,
		EnvironmentName: envName,
	}
	if err := client.SetStatus(ctx, evt); err != nil {
		repositoriesmanager.RetryEvent(&evt, err, store)
		return fmt.Errorf("sendEvent> err:%s", err)
	}

	//Check if this branch and this commit is a pullrequest
	prs, err := client.PullRequests(ctx, node.Context.Application.RepositoryFullname)
	if err != nil {
		log.Error("sendVCSEventStatus> unable to get pull requests on repo %s: %v", node.Context.Application.RepositoryFullname, err)
		return nil
	}

	//Send comment on pull request
	for _, pr := range prs {
		if pr.Head.Branch.DisplayID == nodeRun.VCSBranch && pr.Head.Branch.LatestCommit == nodeRun.VCSHash {
			if nodeRun.Status != sdk.StatusFail.String() {
				continue
			}
			report, err := nodeRun.Report()
			if err != nil {
				log.Error("sendVCSEventStatus> unable to compute node run report%v", err)
				return nil
			}
			if err := client.PullRequestComment(ctx, node.Context.Application.RepositoryFullname, pr.ID, report); err != nil {
				log.Error("sendVCSEventStatus> unable to send PR report%v", err)
				return nil
			}
		}
	}

	return nil
}
