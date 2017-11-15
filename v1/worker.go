package machinery

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RichardKnop/machinery/v1/backends"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/retry"
	"github.com/RichardKnop/machinery/v1/tasks"
)

// Worker represents a single worker process
type Worker struct {
	server      *Server
	ConsumerTag string
	Concurrency int
}

// Launch starts a new worker process. The worker subscribes
// to the default queue and processes incoming registered tasks
func (worker *Worker) Launch() error {
	errorsChan := make(chan error)

	worker.LaunchAsync(errorsChan)

	return <-errorsChan
}

// LaunchAsync is a non blocking version of Launch
func (worker *Worker) LaunchAsync(errorsChan chan<- error) {
	cnf := worker.server.GetConfig()
	broker := worker.server.GetBroker()

	// Log some useful information about woorker configuration
	log.INFO.Printf("Launching a worker with the following settings:")
	log.INFO.Printf("- Broker: %s", cnf.Broker)
	log.INFO.Printf("- DefaultQueue: %s", cnf.DefaultQueue)
	log.INFO.Printf("- ResultBackend: %s", cnf.ResultBackend)
	if cnf.AMQP != nil {
		log.INFO.Printf("- AMQP: %s", cnf.AMQP.Exchange)
		log.INFO.Printf("  - Exchange: %s", cnf.AMQP.Exchange)
		log.INFO.Printf("  - ExchangeType: %s", cnf.AMQP.ExchangeType)
		log.INFO.Printf("  - BindingKey: %s", cnf.AMQP.BindingKey)
		log.INFO.Printf("  - PrefetchCount: %d", cnf.AMQP.PrefetchCount)
	}

	// Goroutine to start broker consumption and handle retries when broker connection dies
	go func() {
		for {
			retry, err := broker.StartConsuming(worker.ConsumerTag, worker.Concurrency, worker)

			if retry {
				log.WARNING.Printf("Start consuming error: %s", err)
			} else {
				errorsChan <- err // stop the goroutine
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var signalsReceived uint

	// Goroutine Handle SIGINT and SIGTERM signals
	go func() {
		for {
			select {
			case s := <-sig:
				log.WARNING.Printf("Signal received: %v", s)
				signalsReceived++

				if signalsReceived < 2 {
					// After first Ctrl+C start quitting the worker gracefully
					log.WARNING.Print("Waiting for running tasks to finish before shutting down")
					go func() {
						worker.Quit()
						errorsChan <- errors.New("Worker quit gracefully")
					}()
				} else {
					// Abort the program when user hits Ctrl+C second time in a row
					errorsChan <- errors.New("Worker quit abruptly")
				}
			}
		}
	}()
}

// Quit tears down the running worker process
func (worker *Worker) Quit() {
	worker.server.GetBroker().StopConsuming()
}

// Process handles received tasks and triggers success/error callbacks
func (worker *Worker) Process(signature *tasks.Signature) error {
	// If the task is not registered with this worker, do not continue
	// but only return nil as we do not want to restart the worker process
	if !worker.server.IsTaskRegistered(signature.Name) {
		return nil
	}

	taskFunc, err := worker.server.GetRegisteredTask(signature.Name)
	if err != nil {
		return nil
	}

	// Update task state to RECEIVED
	if err = worker.server.GetBackend().SetStateReceived(signature); err != nil {
		return fmt.Errorf("Set state received error: %s", err)
	}

	// Prepare task for processing
	task, err := tasks.New(taskFunc, signature.Args)
	// if this failed, it means the task is malformed, probably has invalid
	// signature, go directly to task failed without checking whether to retry
	if err != nil {
		worker.taskFailed(signature, err)
		return err
	}

	// Update task state to STARTED
	if err = worker.server.GetBackend().SetStateStarted(signature); err != nil {
		return fmt.Errorf("Set state started error: %s", err)
	}

	// Call the task
	results, err := task.Call()
	if err != nil {
		// Let's retry the task
		if signature.RetryCount > 0 {
			return worker.taskRetry(signature)
		}

		return worker.taskFailed(signature, err)
	}

	return worker.taskSucceeded(signature, results)
}

// retryTask decrements RetryCount counter and republishes the task to the queue
func (worker *Worker) taskRetry(signature *tasks.Signature) error {
	// Update task state to RETRY
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("Set state retry error: %s", err)
	}

	// Decrement the retry counter, when it reaches 0, we won't retry again
	signature.RetryCount--

	// Increase retry timeout
	signature.RetryTimeout = retry.FibonacciNext(signature.RetryTimeout)

	// Delay task by signature.RetryTimeout seconds
	eta := time.Now().UTC().Add(time.Second * time.Duration(signature.RetryTimeout))
	signature.ETA = &eta

	log.WARNING.Printf("Task %s failed. Going to retry in %ds.", signature.UUID, signature.RetryTimeout)

	// Send the task back to the queue
	_, err := worker.server.SendTask(signature)
	return err
}

// taskSucceeded updates the task state and triggers success callbacks or a
// chord callback if this was the last task of a group with a chord callback
func (worker *Worker) taskSucceeded(signature *tasks.Signature, taskResults []*tasks.TaskResult) error {
	// Update task state to SUCCESS
	if err := worker.server.GetBackend().SetStateSuccess(signature, taskResults); err != nil {
		return fmt.Errorf("Set state success error: %s", err)
	}

	// Log human readable results of the processed task
	var debugResults = "[]"
	results, err := tasks.ReflectTaskResults(taskResults)
	if err != nil {
		log.WARNING.Print(err)
	} else {
		debugResults = tasks.HumanReadableResults(results)
	}
	log.INFO.Printf("Processed task %s. Results = %s", signature.UUID, debugResults)

	// Trigger success callbacks

	for _, successTask := range signature.OnSuccess {
		if signature.Immutable == false {
			// Pass results of the task to success callbacks
			for _, taskResult := range taskResults {
				successTask.Args = append(successTask.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}

		worker.server.SendTask(successTask)
	}

	// If the task was not part of a group, just return
	if signature.GroupUUID == "" {
		return nil
	}

	// Check if all task in the group has completed
	groupCompleted, err := worker.server.GetBackend().GroupCompleted(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		return fmt.Errorf("Group completed error: %s", err)
	}

	// If the group has not yet completed, just return
	if !groupCompleted {
		return nil
	}

	// Defer purging of group meta queue if we are using AMQP backend
	if worker.hasAMQPBackend() {
		defer worker.server.GetBackend().PurgeGroupMeta(signature.GroupUUID)
	}

	// There is no chord callback, just return
	if signature.ChordCallback == nil {
		return nil
	}

	// Trigger chord callback
	shouldTrigger, err := worker.server.GetBackend().TriggerChord(signature.GroupUUID)
	if err != nil {
		return fmt.Errorf("Trigger chord error: %s", err)
	}

	// Chord has already been triggered
	if !shouldTrigger {
		return nil
	}

	// Get task states
	taskStates, err := worker.server.GetBackend().GroupTaskStates(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		return nil
	}

	// Append group tasks' return values to chord task if it's not immutable
	for _, taskState := range taskStates {
		if !taskState.IsSuccess() {
			return nil
		}

		if signature.ChordCallback.Immutable == false {
			// Pass results of the task to the chord callback
			for _, taskResult := range taskState.Results {
				signature.ChordCallback.Args = append(signature.ChordCallback.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}
	}

	// Send the chord task
	_, err = worker.server.SendTask(signature.ChordCallback)
	if err != nil {
		return err
	}

	return nil
}

// taskFailed updates the task state and triggers error callbacks
func (worker *Worker) taskFailed(signature *tasks.Signature, taskErr error) error {
	// Update task state to FAILURE
	if err := worker.server.GetBackend().SetStateFailure(signature, taskErr.Error()); err != nil {
		return fmt.Errorf("Set state failure error: %s", err)
	}

	log.ERROR.Printf("Failed processing %s. Error = %v", signature.UUID, taskErr)

	// Trigger error callbacks
	for _, errorTask := range signature.OnError {
		// Pass error as a first argument to error callbacks
		args := append([]tasks.Arg{{
			Type:  "string",
			Value: taskErr.Error(),
		}}, errorTask.Args...)
		errorTask.Args = args
		worker.server.SendTask(errorTask)
	}

	return nil
}

// Returns true if the worker uses AMQP backend
func (worker *Worker) hasAMQPBackend() bool {
	_, ok := worker.server.GetBackend().(*backends.AMQPBackend)
	return ok
}
