package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// StartWorkflowRequest kicks off a workflow instance. The server
// validates that WorkflowName exists in the IR checkpoint, inserts
// an atlantis.workflow_instances row, and enqueues the first step's
// job. The workflow engine (internal/jobs/workflow.go) advances
// subsequent steps as jobs complete.
type StartWorkflowRequest struct {
	WorkflowName string          `json:"WorkflowName"`
	State        json.RawMessage `json:"State,omitempty"`
	SubmittedBy  string          `json:"SubmittedBy,omitempty"`
}

type StartWorkflowResponse struct {
	WorkflowID string `json:"WorkflowID"`
}

type GetWorkflowStatusRequest struct {
	WorkflowID string `json:"WorkflowID"`
}

type WorkflowStatus struct {
	WorkflowID   string `json:"WorkflowID"`
	WorkflowName string `json:"WorkflowName"`
	Status       string `json:"Status"`
	CurrentStep  string `json:"CurrentStep,omitempty"`
	StartedAt    string `json:"StartedAt"`
	CompletedAt  string `json:"CompletedAt,omitempty"`
	ErrorMsg     string `json:"ErrorMsg,omitempty"`
	SubmittedBy  string `json:"SubmittedBy,omitempty"`
}

type GetWorkflowStatusResponse struct {
	Found    bool           `json:"Found"`
	Workflow WorkflowStatus `json:"Workflow,omitempty"`
}

// StartWorkflow creates a workflow instance and enqueues the first
// step's job. Returns the instance id for monitoring.
func (s *Service) StartWorkflow(ctx context.Context, req StartWorkflowRequest) (*StartWorkflowResponse, error) {
	if !s.allowApplyMutation {
		return nil, errors.New("admin: workflow submission disabled")
	}
	if req.WorkflowName == "" {
		return nil, errors.New("admin: WorkflowName is required")
	}

	ir, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	if ir == nil {
		return nil, errors.New("admin: no IR checkpoint applied")
	}

	var found bool
	for _, wf := range ir.Workflows {
		if wf.ID() == req.WorkflowName {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("admin: unknown workflow %q", req.WorkflowName)
	}

	state := req.State
	if len(state) == 0 {
		state = json.RawMessage("{}")
	}

	var id int64
	err = s.pool.QueryRow(ctx, `
INSERT INTO atlantis.workflow_instances (workflow_name, state, submitted_by)
VALUES ($1, $2, $3)
RETURNING id`, req.WorkflowName, []byte(state), req.SubmittedBy).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("insert workflow: %w", err)
	}

	// The workflow engine's advanceWorkflow handles enqueuing the
	// first step. We trigger it by calling advance with current_step
	// empty (which means "start from step 0").
	// Import the engine indirectly: the admin package can't import
	// internal/jobs without a cycle. Instead, we rely on the
	// StartWorkflow caller (the operator or SDK) to know that the
	// workflow engine's post-insert hook fires on the worker side.
	//
	// Enqueue the first step here so the workflow
	// starts immediately without waiting for a trigger.
	for _, wf := range ir.Workflows {
		if wf.ID() != req.WorkflowName || len(wf.Steps) == 0 {
			continue
		}
		step := wf.Steps[0]
		argsJSON, merr := buildWorkflowStepArgs(step, state)
		if merr != nil {
			return nil, fmt.Errorf("build first step args: %w", merr)
		}
		_, err = s.pool.Exec(ctx, `
INSERT INTO atlantis.jobs (job_name, queue, args, max_retries, timeout_ms, submitted_by, workflow_id, workflow_step)
VALUES ($1, 'default', $2, 3, 1800000, $3, $4, $5)`,
			step.TargetJobID, argsJSON, "workflow:"+req.WorkflowName, id, step.Name)
		if err != nil {
			return nil, fmt.Errorf("enqueue first step: %w", err)
		}
		_, _ = s.pool.Exec(ctx,
			`UPDATE atlantis.workflow_instances SET current_step=$1 WHERE id=$2`, step.Name, id)
		break
	}

	return &StartWorkflowResponse{WorkflowID: fmt.Sprintf("%d", id)}, nil
}

func buildWorkflowStepArgs(step dsl.WorkflowStepIR, stateJSON json.RawMessage) ([]byte, error) {
	var state map[string]any
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(step.Args))
	for _, a := range step.Args {
		if a.Value == nil {
			continue
		}
		switch a.Value.Kind {
		case dsl.ExprArg:
			out[a.Name] = state[a.Value.ArgName]
		case dsl.ExprLiteralStr:
			out[a.Name] = a.Value.LitStr
		case dsl.ExprLiteralInt:
			out[a.Name] = a.Value.LitInt
		case dsl.ExprLiteralBool:
			out[a.Name] = a.Value.LitBool
		}
	}
	return json.Marshal(out)
}

// GetWorkflowStatus reads one workflow instance.
func (s *Service) GetWorkflowStatus(ctx context.Context, req GetWorkflowStatusRequest) (*GetWorkflowStatusResponse, error) {
	if req.WorkflowID == "" {
		return nil, errors.New("admin: WorkflowID is required")
	}
	var (
		ws          WorkflowStatus
		id          int64
		startedAt   time.Time
		completedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, workflow_name, status, COALESCE(current_step, ''), started_at, completed_at,
       COALESCE(error_msg, ''), COALESCE(submitted_by, '')
FROM atlantis.workflow_instances WHERE id = $1`, req.WorkflowID).Scan(
		&id, &ws.WorkflowName, &ws.Status, &ws.CurrentStep,
		&startedAt, &completedAt, &ws.ErrorMsg, &ws.SubmittedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &GetWorkflowStatusResponse{Found: false}, nil
		}
		return nil, err
	}
	ws.WorkflowID = fmt.Sprintf("%d", id)
	ws.StartedAt = startedAt.UTC().Format(time.RFC3339)
	ws.CompletedAt = formatNullable(completedAt)
	return &GetWorkflowStatusResponse{Found: true, Workflow: ws}, nil
}
