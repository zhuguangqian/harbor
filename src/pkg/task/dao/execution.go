// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dao

import (
	"context"
	"fmt"
	"github.com/goharbor/harbor/src/lib/log"

	"github.com/goharbor/harbor/src/jobservice/job"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/lib/orm"
	"github.com/goharbor/harbor/src/lib/q"
)

// ExecutionDAO is the data access object interface for execution
type ExecutionDAO interface {
	// Count returns the total count of executions according to the query
	Count(ctx context.Context, query *q.Query) (count int64, err error)
	// List the executions according to the query
	List(ctx context.Context, query *q.Query) (executions []*Execution, err error)
	// Get the specified execution
	Get(ctx context.Context, id int64) (execution *Execution, err error)
	// Create an execution
	Create(ctx context.Context, execution *Execution) (id int64, err error)
	// Update the specified execution. Only the properties specified by "props" will be updated if it is set
	Update(ctx context.Context, execution *Execution, props ...string) (err error)
	// Delete the specified execution
	Delete(ctx context.Context, id int64) (err error)
	// GetMetrics returns the task metrics for the specified execution
	GetMetrics(ctx context.Context, id int64) (metrics *Metrics, err error)
	// RefreshStatus refreshes the status of the specified execution according to it's tasks. If it's status
	// is final, update the end time as well
	RefreshStatus(ctx context.Context, id int64) (err error)
}

// NewExecutionDAO returns an instance of ExecutionDAO
func NewExecutionDAO() ExecutionDAO {
	return &executionDAO{
		taskDAO: NewTaskDAO(),
	}
}

type executionDAO struct {
	taskDAO TaskDAO
}

func (e *executionDAO) Count(ctx context.Context, query *q.Query) (int64, error) {
	if query != nil {
		// ignore the page number and size
		query = &q.Query{
			Keywords: query.Keywords,
		}
	}
	qs, err := orm.QuerySetter(ctx, &Execution{}, query)
	if err != nil {
		return 0, err
	}
	return qs.Count()
}

func (e *executionDAO) List(ctx context.Context, query *q.Query) ([]*Execution, error) {
	executions := []*Execution{}
	qs, err := orm.QuerySetter(ctx, &Execution{}, query)
	if err != nil {
		return nil, err
	}
	qs = qs.OrderBy("-StartTime")
	if _, err = qs.All(&executions); err != nil {
		return nil, err
	}
	return executions, nil
}

func (e *executionDAO) Get(ctx context.Context, id int64) (*Execution, error) {
	execution := &Execution{
		ID: id,
	}
	ormer, err := orm.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := ormer.Read(execution); err != nil {
		if e := orm.AsNotFoundError(err, "execution %d not found", id); e != nil {
			err = e
		}
		return nil, err
	}
	return execution, nil
}

func (e *executionDAO) Create(ctx context.Context, execution *Execution) (int64, error) {
	ormer, err := orm.FromContext(ctx)
	if err != nil {
		return 0, err
	}
	return ormer.Insert(execution)
}

func (e *executionDAO) Update(ctx context.Context, execution *Execution, props ...string) error {
	ormer, err := orm.FromContext(ctx)
	if err != nil {
		return err
	}
	n, err := ormer.Update(execution, props...)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.NotFoundError(nil).WithMessage("execution %d not found", execution.ID)
	}
	return nil
}

func (e *executionDAO) Delete(ctx context.Context, id int64) error {
	ormer, err := orm.FromContext(ctx)
	if err != nil {
		return err
	}
	n, err := ormer.Delete(&Execution{
		ID: id,
	})
	if err != nil {
		if e := orm.AsForeignKeyError(err,
			"the execution %d is referenced by other resources", id); e != nil {
			err = e
		}
		return err
	}
	if n == 0 {
		return errors.NotFoundError(nil).WithMessage("execution %d not found", id)
	}
	return nil
}

func (e *executionDAO) GetMetrics(ctx context.Context, id int64) (*Metrics, error) {
	scs, err := e.taskDAO.ListStatusCount(ctx, id)
	if err != nil {
		return nil, err
	}
	metrics := &Metrics{}
	if len(scs) == 0 {
		return metrics, nil
	}

	for _, sc := range scs {
		switch sc.Status {
		case job.SuccessStatus.String():
			metrics.SuccessTaskCount = sc.Count
		case job.ErrorStatus.String():
			metrics.ErrorTaskCount = sc.Count
		case job.PendingStatus.String():
			metrics.PendingTaskCount = sc.Count
		case job.RunningStatus.String():
			metrics.RunningTaskCount = sc.Count
		case job.ScheduledStatus.String():
			metrics.ScheduledTaskCount = sc.Count
		case job.StoppedStatus.String():
			metrics.StoppedTaskCount = sc.Count
		default:
			log.Errorf("unknown task status: %s", sc.Status)
		}
	}
	metrics.TaskCount = metrics.SuccessTaskCount + metrics.ErrorTaskCount +
		metrics.PendingTaskCount + metrics.RunningTaskCount +
		metrics.ScheduledTaskCount + metrics.StoppedTaskCount
	return metrics, nil
}
func (e *executionDAO) RefreshStatus(ctx context.Context, id int64) error {
	// as the status of the execution can be refreshed by multiple operators concurrently
	// we use the optimistic locking to avoid the conflict and retry 5 times at most
	for i := 0; i < 5; i++ {
		retry, err := e.refreshStatus(ctx, id)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("failed to refresh the status of the execution %d after %d retries", id, 5)
}

func (e *executionDAO) refreshStatus(ctx context.Context, id int64) (bool, error) {
	execution, err := e.Get(ctx, id)
	if err != nil {
		return false, err
	}
	metrics, err := e.GetMetrics(ctx, id)
	if err != nil {
		return false, err
	}
	// no task, return directly
	if metrics.TaskCount == 0 {
		return false, nil
	}

	var status string
	if metrics.PendingTaskCount > 0 || metrics.RunningTaskCount > 0 || metrics.ScheduledTaskCount > 0 {
		status = job.RunningStatus.String()
	} else if metrics.ErrorTaskCount > 0 {
		status = job.ErrorStatus.String()
	} else if metrics.StoppedTaskCount > 0 {
		status = job.StoppedStatus.String()
	} else if metrics.SuccessTaskCount > 0 {
		status = job.SuccessStatus.String()
	}

	ormer, err := orm.FromContext(ctx)
	if err != nil {
		return false, err
	}
	sql := `update execution set status = ?, revision = revision+1 where id = ? and revision = ?`
	result, err := ormer.Raw(sql, status, id, execution.Revision).Exec()
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	// if the count of affected rows is 0, that means the execution is updating by others, retry
	if n == 0 {
		return true, nil
	}

	/* this is another solution to solve the concurrency issue for refreshing the execution status
	// set a score for each status:
	// 		pending, running, scheduled - 4
	// 		error - 3
	//		stopped - 2
	//		success - 1
	// and set the status of record with highest score as the status of execution
	sql := `with status_score as (
				select status,
					case
						when status='%s' or status='%s' or status='%s' then 4
						when status='%s' then 3
						when status='%s' then 2
						when status='%s' then 1
						else 0
					end as score
				from task
				where execution_id=?
				group by status
			)
			update execution
			set status=(
				select
					case
						when max(score)=4 then '%s'
						when max(score)=3 then '%s'
						when max(score)=2 then '%s'
						when max(score)=1 then '%s'
						when max(score)=0 then ''
					end as status
				from status_score)
			where id = ?`
	sql = fmt.Sprintf(sql, job.PendingStatus.String(), job.RunningStatus.String(), job.ScheduledStatus.String(),
		job.ErrorStatus.String(), job.StoppedStatus.String(), job.SuccessStatus.String(),
		job.RunningStatus.String(), job.ErrorStatus.String(), job.StoppedStatus.String(), job.SuccessStatus.String())
	if _, err = ormer.Raw(sql, id, id).Exec(); err != nil {
		return err
	}
	*/

	// update the end time if the status is final, otherwise set the end time as NULL, this is useful
	// for retrying jobs
	sql = `update execution
			set end_time = (
				case
					when status='%s' or status='%s' or status='%s' then  (
						select max(end_time)
						from task
						where execution_id=?)
					else NULL
				end)
			where id=?`
	sql = fmt.Sprintf(sql, job.ErrorStatus.String(), job.StoppedStatus.String(), job.SuccessStatus.String())
	_, err = ormer.Raw(sql, id, id).Exec()
	return false, err
}
