package common

import (
	"context"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	rdkutils "go.viam.com/rdk/utils"
)

// Primary defines the primary sensor for the failover.
type Primary struct {
	workers         rdkutils.StoppableWorkers
	logger          logging.Logger
	primarySensor   resource.Sensor
	pollPrimaryChan chan bool
	timeout         int

	mu         sync.Mutex
	usePrimary bool

	calls []Call
}

func CreatePrimary(ctx context.Context,
	timeout int,
	logger logging.Logger,
	primarySensor resource.Sensor,
	calls []Call,
) *Primary {
	primary := &Primary{
		workers:         rdkutils.NewStoppableWorkers(),
		pollPrimaryChan: make(chan bool),
		usePrimary:      true,
		timeout:         timeout,
		primarySensor:   primarySensor,
		logger:          logger,
		calls:           calls,
	}

	// Start goroutine to check health of the primary sensor
	primary.PollPrimaryForHealth()

	// TryAllReadings to determine the health of the primary sensor and set the usePrimary flag accordingly.
	primary.TryAllReadings(ctx)

	return primary
}

func (p *Primary) UsePrimary() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usePrimary
}

func (p *Primary) setUsePrimary(val bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usePrimary = val
}

// TryAllReadings checks that all functions on primary are working,
// if not tell the goroutine to start polling for health and don't use the primary.
func (p *Primary) TryAllReadings(ctx context.Context) {
	err := CallAllFunctions(ctx, p.primarySensor, p.timeout, nil, p.calls)
	if err != nil {
		p.logger.Warnf("primary sensor failed: %s", err.Error())
		p.setUsePrimary(false)
		p.pollPrimaryChan <- true
	}
}

// TryPrimary is a helper function to call a reading from the primary sensor and start polling if it fails.
func TryPrimary[T any](ctx context.Context,
	s *Primary,
	extra map[string]any,
	call Call,
) (T, error) {
	readings, err := TryReadingOrFail(ctx, s.timeout, s.primarySensor, call, extra)
	if err == nil {
		reading := any(readings).(T)
		return reading, nil
	}
	var zero T

	// upon error of the last working sensor, log the error.
	s.logger.Warnf("primary sensor failed: %s", err.Error())

	// If the primary failed, tell the goroutine to start checking the health.
	s.pollPrimaryChan <- true
	s.setUsePrimary(false)
	return zero, err
}

// PollPrimaryForHealth starts a goroutine and waits for data to come into the pollPrimaryChan.
// Then, it calls all APIs on the primary sensor until they are all successful and updates the
// UsePrimary flag.
func (p *Primary) PollPrimaryForHealth() {
	// poll every 100 ms.
	ticker := time.NewTicker(time.Millisecond * 100)
	p.workers.AddWorkers(func(ctx context.Context) {
		for {
			select {
			// wait for data to come into the channel before polling.
			case <-ctx.Done():
				return
			case <-p.pollPrimaryChan:
			}
			// label for loop so we can break out of it later.
		L:
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					err := CallAllFunctions(ctx, p.primarySensor, p.timeout, nil, p.calls)
					// Primary succeeded, set flag to true
					if err == nil {
						p.setUsePrimary(true)
						break L
					}
				}
			}
		}
	})
}

func (p *Primary) Close() {
	close(p.pollPrimaryChan)
	if p.workers != nil {
		p.workers.Stop()
	}
}
