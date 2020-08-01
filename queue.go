package orcaq

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/twinj/uuid"

	"github.com/xujiajun/nutsdb"
)

const (
	jobsBucketName = "Jobs"
)

// Queue represents a queue
type Queue struct {
	//ID is a unique identifier for a Queue
	ID string
	//db represents a handle to a key/value store
	db *nutsdb.DB
	//notifier is a chan used to signal workers there is a job to begin working
	notifier chan []byte
	//workeres is a list of *Workers
	workers []*Worker
	//shutdownFuncs are context.CancleFuncs used to signal graceful shutdown
	shutdownFuncs []context.CancelFunc
	//wg is used to help gracefully shutdown workers
	wg *sync.WaitGroup

	//PollRate the duration to Sleep each worker before checking the queue for jobs again
	//queue for jobs again.
	//Default: 500 milliseconds
	PollRate time.Duration
}

//Init creates a connection to the internal database and initializes the Queue type
//filepath must be a valid path to a file. It cannot be shared between instances of
//a Queue. If the  file cannot be opened r/w, an error is returned.
func Init(filepath string) (*Queue, error) {
	q := &Queue{ID: filepath, PollRate: time.Duration(500 * time.Millisecond)}

	// create a new db
	opt := nutsdb.DefaultOptions
	opt.Dir = filepath
	db, err := nutsdb.Open(opt)
	if err != nil {
		log.Print(err)
		return nil, err
	}
	q.db = db

	// Make notification channels
	c := make(chan []byte, 1000) //TODO: channel probably isn't the best way to handle the queue buffer
	q.notifier = c
	q.workers = make([]*Worker, 0)
	q.shutdownFuncs = make([]context.CancelFunc, 0)
	var wg sync.WaitGroup
	q.wg = &wg

	//resume stopped jobs, clean completed, failed jobs
	err = q.processJobs()
	if err != nil {
		log.Printf("Unable to resume jobs from bucket: %s", err)
	}
	return q, nil
}

//Close attempts to gracefully shutdown all workers in a queue and shutdown the db connection
func (q *Queue) Close() error {
	for _, f := range q.shutdownFuncs {
		f()
	}
	q.wg.Wait()
	q.notifier = nil
	q.workers = nil
	q.shutdownFuncs = nil
	return q.db.Close()
}

//registerWorkerWithContext contains the main loop for all Workers.
func (q *Queue) registerWorkerWithContext(ctx context.Context, w Worker) {
	q.workers = append(q.workers, &w)
	q.wg.Add(1)
	log.Printf("Registering worker with ID: %s", w.ID())
	//The big __main loop__ for workers.
	go func() {
		log.Printf("Starting up new worker...")
		var jobID []byte
		for {
			// receive a notification from the queue chan
			select {
			case <-ctx.Done():
				log.Printf("Received signal to shutdown worker. Exiting.")
				q.wg.Done()
				return
			case jobID = <-q.notifier:
				log.Printf("Received job id %v", string(jobID))
				err := q.updateJobStatus(jobID, Uack, fmt.Sprintf("Picked up by %s", w.ID()))
				if err != nil {
					log.Printf("Unable to update job status: %s", err)
					continue
				}
				//If subsequent calls to updateJobStatus fail, the whole thing is probably hosed and
				//it should probably do something more drastic for error handling.
				job, err := q.GetJobByID(jobID)
				if err != nil {
					log.Printf("Error processing job: %s", err)
					q.updateJobStatus(jobID, Failed, err.Error())
					continue
				}
				// Call the worker func handling this job
				err = w.DoWork(ctx, job)
				if err != nil {
					_, ok := err.(RecoverableWorkerError)
					if ok {
						//temporary error, retry
						log.Printf("Received temporary error: %s. Retrying...", err.Error())
						q.updateJobStatus(jobID, Nack, err.Error())
					} else {
						log.Printf("Permanent error received from worker: %s", err)
						//permanent error, mark as failed
						q.updateJobStatus(jobID, Failed, err.Error())
					}
				} else {
					q.updateJobStatus(jobID, Ack, "Complete")
				}
				log.Printf("Finished processing job %v", string(jobID))
			default:
				// log.Printf("Worker: %s. No message to queue. Sleeping 500ms", w.ID())
				time.Sleep(q.PollRate)
			}
		}
	}()
}

//RegisterWorker registers a Worker to handle queued Jobs
func (q *Queue) RegisterWorker(w Worker) {
	baseCtx := context.Background()
	ctx, cancelFunc := context.WithCancel(baseCtx)
	q.shutdownFuncs = append(q.shutdownFuncs, cancelFunc)
	q.registerWorkerWithContext(ctx, w)
}

//PushBytes wraps arbitrary binary data in a job and pushes it onto the queue
func (q *Queue) PushBytes(d []byte) ([]byte, error) {
	job := &Job{
		Status:     Uack,
		Data:       d,
		RetryCount: 0,
	}
	return q.PushJob(job)
}

//PushJob pushes a job to the queue and notifies workers
// Job.ID is always overwritten
func (q *Queue) PushJob(j *Job) ([]byte, error) {
	err := q.db.Update(func(tx *nutsdb.Tx) error {
		j.ID = []byte(uuid.NewV4().String())
		log.Printf("Storing job %v for processing", string(j.ID))
		err := tx.Put(jobsBucketName, j.ID, j.Bytes(), 0) // setting this to 0 means never expires.
		// NOTE: we can support jobs with a ttl. If the ttl expires, then the job is removed from the queue.
		return err
	})
	if err != nil {
		log.Printf("Unable to push job to queue: %s", err)
		return nil, err
	}
	q.notifier <- j.ID
	return j.ID, nil
}

//GetJobByID returns a pointer to a Job based on the primary key identifier id
func (q *Queue) GetJobByID(id []byte) (*Job, error) {
	var job *Job
	err := q.db.View(func(tx *nutsdb.Tx) error {
		e, err := tx.Get(jobsBucketName, id)
		if err != nil {
			return err
		}
		job = DecodeJob(e.Value)
		return nil
	})
	return job, err
}

//updateJobStatus updates the processing status of a job
func (q *Queue) updateJobStatus(id []byte, status JobStatus, message string) error {
	err := q.db.Update(func(tx *nutsdb.Tx) error {
		e, err := tx.Get(jobsBucketName, id)
		if err != nil {
			return err
		}
		job := DecodeJob(e.Value)
		job.Status = status
		job.Message = message
		if status == Nack {
			job.RetryCount++
		}
		return tx.Put(jobsBucketName, job.ID, job.Bytes(), 0)
	})

	if status == Nack && err == nil {
		q.notifier <- id
	}
	return err
}

// processJobs loops through all jobs marked as completed or failed and deletes them from the database
// Warning: this is destructive, that job data is definitely done if you call this function.
func (q *Queue) processJobs() error {
	return q.db.Update(func(tx *nutsdb.Tx) error {
		entries, err := tx.GetAll(jobsBucketName)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			job := DecodeJob(entry.Value)
			switch job.Status {
			case Uack:
			case Nack:
				break
			case Failed:
				err := tx.Delete(jobsBucketName, job.ID)
				if err != nil {
					log.Errorf("Unable to delete failed job %v from queue.", string(job.ID))
					return err
				}
				log.Infof("removed failed job %v from queue", string(job.ID))
				break
			case Ack:
				err := tx.Delete(jobsBucketName, job.ID)
				if err != nil {
					log.Errorf("Unable to delete completed job %v from queue.", string(job.ID))
					return err
				}
				log.Infof("removed completed job %v from queue", string(job.ID))
				break
			}
		}
		return nil
	})
}

// ListJobs will return a list of jobs within the queue
func (q *Queue) ListJobs() (map[string]interface{}, error) {
	r := map[string]interface{}{}
	err := q.db.View(func(tx *nutsdb.Tx) error {
		entries, err := tx.GetAll(jobsBucketName)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			job := DecodeJob(entry.Value)
			r[string(job.ID)] = job
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}
