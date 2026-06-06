package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// WorkflowEngine advances and compensates workflow instances. The
// worker calls OnJobComplete / OnJobFailed after each claim
// completes; the engine checks if the finished job is part of a
// workflow and acts accordingly.
//
// The engine does NOT run its own goroutine. It piggybacks on the
// worker's drain loop, so a workflow advances as fast as the worker
// drains jobs. Each advancement enqueues the next step's job into
// atlantis.jobs; the worker picks it up on its next claim cycle.
type WorkflowEngine struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewWorkflowEngine constructs the engine. Called from cmd/server/main.go
// alongside the worker pool.
func NewWorkflowEngine(pool *pgxpool.Pool, logger *slog.Logger) *WorkflowEngine {
	return &WorkflowEngine{pool: pool, logger: logger}
}

// OnJobComplete is called by the worker after a job's handler returns
// nil. If the job is part of a workflow (workflow_id != NULL), the
// engine advances to the next step or marks the workflow complete.
func (e *WorkflowEngine) OnJobComplete(ctx context.Context, jobID int64) {
	wfID, stepName, err := e.lookupWorkflowJob(ctx, jobID)
	if err != nil || wfID == 0 {
		return
	}
	if err := e.recordStepComplete(ctx, wfID, stepName, jobID); err != nil {
		e.logger.Warn("workflow: record step complete", "workflow_id", wfID, "step", stepName, "err", err)
		return
	}
	if err := e.advanceWorkflow(ctx, wfID); err != nil {
		e.logger.Warn("workflow: advance", "workflow_id", wfID, "err", err)
	}
}

// OnJobFailed is called by the worker when a job exhausts its retries
// and moves to the DLQ. If the job is part of a workflow, the engine
// starts compensation.
func (e *WorkflowEngine) OnJobFailed(ctx context.Context, jobID int64, jobErr string) {
	wfID, stepName, err := e.lookupWorkflowJob(ctx, jobID)
	if err != nil || wfID == 0 {
		return
	}
	e.logger.Info("workflow: step failed, starting compensation", "workflow_id", wfID, "step", stepName)
	if err := e.compensateWorkflow(ctx, wfID, jobErr); err != nil {
		e.logger.Error("workflow: compensate", "workflow_id", wfID, "err", err)
	}
}

func (e *WorkflowEngine) lookupWorkflowJob(ctx context.Context, jobID int64) (wfID int64, stepName string, err error) {
	// Check the live jobs table first, then dead (the job may have
	// already been moved to DLQ by the time we're called).
	err = e.pool.QueryRow(ctx,
		`SELECT COALESCE(workflow_id, 0), COALESCE(workflow_step, '') FROM atlantis.jobs WHERE id = $1`, jobID).Scan(&wfID, &stepName)
	if err != nil {
		// Try the dead table.
		// Jobs_dead doesn't have workflow columns yet, so this is a
		// no-op for now. When we add workflow_id/step to jobs_dead,
		// this path will activate.
		return 0, "", nil
	}
	return wfID, stepName, nil
}

func (e *WorkflowEngine) recordStepComplete(ctx context.Context, wfID int64, stepName string, jobID int64) error {
	_, err := e.pool.Exec(ctx, `
INSERT INTO atlantis.workflow_step_history (workflow_id, step_name, job_id)
VALUES ($1, $2, $3)`, wfID, stepName, jobID)
	return err
}

// advanceWorkflow loads the workflow IR from the checkpoint, finds
// the next step after current_step, and enqueues its job. If no
// more steps remain, marks the workflow complete.
func (e *WorkflowEngine) advanceWorkflow(ctx context.Context, wfID int64) error {
	var name, currentStep, stateRaw string
	var status string
	err := e.pool.QueryRow(ctx,
		`SELECT workflow_name, COALESCE(current_step, ''), status, state::text FROM atlantis.workflow_instances WHERE id = $1`,
		wfID).Scan(&name, &currentStep, &status, &stateRaw)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	if status != "running" {
		return nil
	}

	wf, err := e.loadWorkflowIR(ctx, name)
	if err != nil {
		return err
	}
	if wf == nil {
		return fmt.Errorf("workflow %s not found in IR", name)
	}

	// Find the next step.
	nextIdx := -1
	if currentStep == "" {
		nextIdx = 0
	} else {
		for i, s := range wf.Steps {
			if s.Name == currentStep && i+1 < len(wf.Steps) {
				nextIdx = i + 1
				break
			}
		}
	}

	if nextIdx < 0 || nextIdx >= len(wf.Steps) {
		// No more steps; workflow complete.
		_, err := e.pool.Exec(ctx,
			`UPDATE atlantis.workflow_instances SET status='complete', completed_at=now() WHERE id=$1`, wfID)
		return err
	}

	step := wf.Steps[nextIdx]
	argsJSON, err := e.buildStepArgs(step.Args, stateRaw)
	if err != nil {
		return fmt.Errorf("build args for step %s: %w", step.Name, err)
	}

	// Enqueue the step's job with workflow correlation.
	_, err = e.pool.Exec(ctx, `
INSERT INTO atlantis.jobs (job_name, queue, args, max_retries, timeout_ms, submitted_by, workflow_id, workflow_step)
VALUES ($1, 'default', $2, 3, 1800000, $3, $4, $5)`,
		step.TargetJobID, argsJSON, "workflow:"+name, wfID, step.Name)
	if err != nil {
		return fmt.Errorf("enqueue step %s: %w", step.Name, err)
	}

	_, err = e.pool.Exec(ctx,
		`UPDATE atlantis.workflow_instances SET current_step=$1 WHERE id=$2`, step.Name, wfID)
	return err
}

// compensateWorkflow runs compensations for completed steps in reverse.
func (e *WorkflowEngine) compensateWorkflow(ctx context.Context, wfID int64, failureMsg string) error {
	_, err := e.pool.Exec(ctx,
		`UPDATE atlantis.workflow_instances SET status='compensating', error_msg=$1 WHERE id=$2`, failureMsg, wfID)
	if err != nil {
		return err
	}

	var name, stateRaw string
	err = e.pool.QueryRow(ctx,
		`SELECT workflow_name, state::text FROM atlantis.workflow_instances WHERE id=$1`, wfID).Scan(&name, &stateRaw)
	if err != nil {
		return err
	}

	wf, err := e.loadWorkflowIR(ctx, name)
	if err != nil {
		return err
	}
	if wf == nil {
		return fmt.Errorf("workflow %s not found in IR", name)
	}

	// Get completed steps in reverse order.
	rows, err := e.pool.Query(ctx,
		`SELECT step_name FROM atlantis.workflow_step_history WHERE workflow_id=$1 ORDER BY completed_at DESC`, wfID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var completedSteps []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		completedSteps = append(completedSteps, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// For each completed step (reverse order), find its compensation
	// and enqueue the compensation job.
	compByStep := make(map[string]*dsl.WorkflowCompIR, len(wf.Compensations))
	for i := range wf.Compensations {
		compByStep[wf.Compensations[i].StepName] = &wf.Compensations[i]
	}
	for _, stepName := range completedSteps {
		comp, ok := compByStep[stepName]
		if !ok {
			continue
		}
		argsJSON, err := e.buildStepArgs(comp.Args, stateRaw)
		if err != nil {
			e.logger.Warn("workflow: compensate args", "step", stepName, "err", err)
			continue
		}
		_, err = e.pool.Exec(ctx, `
INSERT INTO atlantis.jobs (job_name, queue, args, max_retries, timeout_ms, submitted_by)
VALUES ($1, 'default', $2, 1, 1800000, $3)`,
			comp.TargetJobID, argsJSON, "compensate:"+name)
		if err != nil {
			e.logger.Warn("workflow: enqueue compensation", "step", stepName, "err", err)
		}
	}

	_, err = e.pool.Exec(ctx,
		`UPDATE atlantis.workflow_instances SET status='failed', completed_at=now() WHERE id=$1`, wfID)
	return err
}

func (e *WorkflowEngine) loadWorkflowIR(ctx context.Context, name string) (*dsl.Workflow, error) {
	var irRaw []byte
	err := e.pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&irRaw)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	ir, err := dsl.DecodeJSONIR(irRaw)
	if err != nil {
		return nil, fmt.Errorf("decode ir: %w", err)
	}
	for i := range ir.Workflows {
		if ir.Workflows[i].ID() == name {
			return &ir.Workflows[i], nil
		}
	}
	return nil, nil
}

// buildStepArgs resolves step arg expressions against the workflow
// state. For now only $name (state field) and string literals are
// supported; the resolver pulls $name from the state JSON.
func (e *WorkflowEngine) buildStepArgs(args []dsl.EnqueueAssignmentIR, stateJSON string) ([]byte, error) {
	var state map[string]any
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}

	out := make(map[string]any, len(args))
	for _, a := range args {
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
		default:
			out[a.Name] = nil
		}
	}
	return json.Marshal(out)
}
