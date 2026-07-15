package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inerplat/idleloom/internal/native/devruntime"
)

func TestBatchProcessWritesStructuredResultAndStopsRunner(t *testing.T) {
	underlying := &fakeBatchRunner{
		alive:  true,
		result: devruntime.GenerateResponse{Text: "ready", ElapsedMillis: 42},
	}
	var output bytes.Buffer
	process := startBatchProcess(underlying, devruntime.GenerateRequest{Prompt: "hello", MaxTokens: 8}, time.Second, &output)
	waitForBatch(t, process)
	if err := process.WaitError(); err != nil {
		t.Fatal(err)
	}
	if underlying.stopCalls != 1 || underlying.request.Prompt != "hello" || underlying.request.MaxTokens != 8 {
		t.Fatalf("runner state = %#v", underlying)
	}
	if !strings.Contains(output.String(), `"type":"result"`) || !strings.Contains(output.String(), `"text":"ready"`) {
		t.Fatalf("batch output = %q", output.String())
	}
}

func TestStoppingBatchTreatsCancellationAsCleanStop(t *testing.T) {
	underlying := &fakeBatchRunner{alive: true, waitForCancellation: true}
	process := startBatchProcess(underlying, devruntime.GenerateRequest{Prompt: "hello", MaxTokens: 8}, time.Minute, nil)
	if err := process.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if process.Alive() {
		t.Fatal("batch process remained alive after Stop")
	}
}

func TestFailedBatchCanStillBeStoppedForCleanup(t *testing.T) {
	generateErr := errors.New("generation failed")
	underlying := &fakeBatchRunner{alive: true, generateErr: generateErr}
	process := startBatchProcess(underlying, devruntime.GenerateRequest{Prompt: "hello", MaxTokens: 8}, time.Minute, nil)
	waitForBatch(t, process)
	if !errors.Is(process.WaitError(), generateErr) {
		t.Fatalf("WaitError = %v", process.WaitError())
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("cleanup Stop returned workload failure: %v", err)
	}
}

func waitForBatch(t *testing.T, process Process) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for process.Alive() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if process.Alive() {
		t.Fatal("batch process did not complete")
	}
}

type fakeBatchRunner struct {
	mu                  sync.Mutex
	alive               bool
	waitForCancellation bool
	result              devruntime.GenerateResponse
	generateErr         error
	request             devruntime.GenerateRequest
	stopCalls           int
	pid                 int
}

func (p *fakeBatchRunner) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive
}

func (p *fakeBatchRunner) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alive = false
	p.stopCalls++
	return nil
}

func (p *fakeBatchRunner) Generate(ctx context.Context, request devruntime.GenerateRequest) (devruntime.GenerateResponse, error) {
	p.mu.Lock()
	p.request = request
	wait := p.waitForCancellation
	result := p.result
	err := p.generateErr
	p.mu.Unlock()
	if wait {
		<-ctx.Done()
		return devruntime.GenerateResponse{}, ctx.Err()
	}
	return result, err
}

func (p *fakeBatchRunner) Stderr() string { return "" }

func (p *fakeBatchRunner) WaitError() error { return p.generateErr }

func (p *fakeBatchRunner) PID() int { return p.pid }
