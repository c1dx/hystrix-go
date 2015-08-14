package hystrix

import (
	"fmt"
	"sync"
	"time"
)

type runFunc func() error
type fallbackFunc func(error) error

// A CircuitError is an error which models various failure states of execution,
// such as the circuit being open or a timeout.
type CircuitError struct {
	Message string
}

func (e CircuitError) Error() string {
	return "hystrix: " + e.Message
}

var (
	// ErrMaxConcurrency occurs when too many of the same named command are executed at the same time.
	ErrMaxConcurrency = CircuitError{Message: "max concurrency"}
	// ErrCircuitOpen returns when an execution attempt "short circuits". This happens due to the circuit being measured as unhealthy.
	ErrCircuitOpen = CircuitError{Message: "circuit open"}
	// ErrTimeout occurs when the provided function takes too long to execute.
	ErrTimeout = CircuitError{Message: "timeout"}
)

// Go runs your function while tracking the health of previous calls to it.
// If your function begins slowing down or failing repeatedly, we will block
// new calls to it for you to give the dependent service time to repair.
//
// Define a fallback function if you want to define some code to execute during outages.
func Go(name string, run runFunc, fallback fallbackFunc) chan error {
	stop := false
	stopMutex := &sync.Mutex{}
	var ticket *struct{}
	ticketMutex := &sync.Mutex{}

	start := time.Now()

	errChan := make(chan error, 1)
	finished := make(chan bool, 1)
	fallbackOnce := &sync.Once{}

	// dont have methods with explicit params and returns
	// let data come in and out naturally, like with any closure
	// explicit error return to give place for us to kill switch the operation (fallback)

	circuit, _, err := GetCircuit(name)
	if err != nil {
		errChan <- err
		return errChan
	}

	go func() {
		defer func() { finished <- true }()

		// Circuits get opened when recent executions have shown to have a high error rate.
		// Rejecting new executions allows backends to recover, and the circuit will allow
		// new traffic when it feels a healthly state has returned.
		if !circuit.AllowRequest() {
			stopMutex.Lock()
			defer stopMutex.Unlock()
			if stop {
				return
			}
			stop = true
			
			circuit.ReportEvent("short-circuit", start, 0)
			err := tryFallback(fallbackOnce, circuit, start, 0, fallback, ErrCircuitOpen)
			if err != nil {
				errChan <- err
			}
			return
		}

		// As backends falter, requests take longer but don't always fail.
		//
		// When requests slow down but the incoming rate of requests stays the same, you have to
		// run more at a time to keep up. By controlling concurrency during these situations, you can
		// shed load which accumulates due to the increasing ratio of active commands to incoming requests.
		ticketMutex.Lock()
		select {
		case ticket = <-circuit.executorPool.Tickets:
			ticketMutex.Unlock()
		default:
			ticketMutex.Unlock()
			circuit.ReportEvent("rejected", start, 0)
			err := tryFallback(fallbackOnce, circuit, start, 0, fallback, ErrMaxConcurrency)
			if err != nil {
				errChan <- err
			}
			return
		}

		runStart := time.Now()
		runErr := run()
		runDuration := time.Now().Sub(runStart)

		stopMutex.Lock()
		defer stopMutex.Unlock()
		if stop {
			return
		}
		stop = true

		if runErr != nil {
			circuit.ReportEvent("failure", start, runDuration)
			err := tryFallback(fallbackOnce, circuit, start, runDuration, fallback, runErr)
			if err != nil {
				errChan <- err
				return
			}
		}

		circuit.ReportEvent("success", start, runDuration)
	}()

	go func() {
		defer func() {
			ticketMutex.Lock()
			circuit.executorPool.Return(ticket)
			ticketMutex.Unlock()
		}()

		timer := time.NewTimer(getSettings(name).Timeout)
		defer timer.Stop()

		select {
		case <-finished:
		case <-timer.C:
			stopMutex.Lock()
			defer stopMutex.Unlock()

			if !stop {
				stop = true

				circuit.ReportEvent("timeout", start, 0)

				err := tryFallback(fallbackOnce, circuit, start, 0, fallback, ErrTimeout)
				if err != nil {
					errChan <- err
				}
			}
		}
	}()

	return errChan
}

// Do runs your function in a synchronous manner, blocking until either your function succeeds
// or an error is returned, including hystrix circuit errors
func Do(name string, run runFunc, fallback fallbackFunc) error {
	done := make(chan struct{}, 1)

	r := func() error {
		err := run()
		if err != nil {
			return err
		}

		done <- struct{}{}
		return nil
	}

	f := func(e error) error {
		err := fallback(e)
		if err != nil {
			return err
		}

		done <- struct{}{}
		return nil
	}

	var errChan chan error
	if fallback == nil {
		errChan = Go(name, r, nil)
	} else {
		errChan = Go(name, r, f)
	}

	select {
	case <-done:
		return nil
	case err := <-errChan:
		return err
	}
}

func tryFallback(once *sync.Once, circuit *CircuitBreaker, start time.Time, runDuration time.Duration, fallback fallbackFunc, err error) error {
	errors := make(chan error, 1)
	var ran bool

	once.Do(func() {
		ran = true
		if fallback == nil {
			// If we don't have a fallback return the original error.
			errors <- err
			return
		}

		fallbackErr := fallback(err)
		if fallbackErr != nil {
			circuit.ReportEvent("fallback-failure", start, runDuration)
			errors <- fmt.Errorf("fallback failed with '%v'. run error was '%v'", fallbackErr, err)
			return
		}

		circuit.ReportEvent("fallback-success", start, runDuration)

		errors <- nil
	})

	if !ran {
		errors <- nil
	}

	return <-errors
}
