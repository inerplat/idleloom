package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/inerplat/idleloom/internal/native/devruntime"
)

type batchProcess struct {
	process Process
	cancel  context.CancelFunc
	done    chan struct{}

	mu       sync.Mutex
	waitErr  error
	stopping bool
}

type batchResultLog struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	ElapsedMillis int64  `json:"elapsedMillis"`
}

func startBatchProcess(process Process, request devruntime.GenerateRequest, timeout time.Duration, output io.Writer) Process {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	batch := &batchProcess{process: process, cancel: cancel, done: make(chan struct{})}
	if output == nil {
		output = io.Discard
	}
	go func() {
		defer close(batch.done)
		result, generateErr := process.Generate(ctx, request)
		var outputErr error
		if generateErr == nil {
			outputErr = json.NewEncoder(output).Encode(batchResultLog{
				Type: "result", Text: result.Text, ElapsedMillis: result.ElapsedMillis,
			})
		}
		stopErr := process.Stop()
		batch.mu.Lock()
		if batch.stopping && errors.Is(generateErr, context.Canceled) {
			generateErr = nil
		}
		batch.waitErr = errors.Join(generateErr, outputErr, stopErr)
		batch.mu.Unlock()
	}()
	return batch
}

func (p *batchProcess) Alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *batchProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.cancel()
	stopErr := p.process.Stop()
	select {
	case <-p.done:
		return stopErr
	case <-time.After(10 * time.Second):
		return errors.Join(stopErr, fmt.Errorf("native batch process did not stop"))
	}
}

func (p *batchProcess) PID() int {
	if p == nil || p.process == nil {
		return 0
	}
	if process, ok := p.process.(interface{ PID() int }); ok {
		return process.PID()
	}
	return 0
}

func (p *batchProcess) Generate(context.Context, devruntime.GenerateRequest) (devruntime.GenerateResponse, error) {
	return devruntime.GenerateResponse{}, fmt.Errorf("generate is unavailable for batch workloads")
}

func (p *batchProcess) Stderr() string {
	if p == nil || p.process == nil {
		return ""
	}
	return p.process.Stderr()
}

func (p *batchProcess) WaitError() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}
