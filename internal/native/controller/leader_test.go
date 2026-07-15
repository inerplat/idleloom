package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestRunLeaderElectedRunsReconcileAfterAcquiringLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := kubernetesfake.NewClientset()
	var calls atomic.Int32
	err := RunLeaderElected(ctx, client.CoordinationV1(), LeaderOptions{
		Namespace: "system", Name: "controller", Identity: "host-one",
		LeaseDuration: time.Second, RenewDeadline: 700 * time.Millisecond, RetryPeriod: 100 * time.Millisecond,
		Reconcile: func(leaderCtx context.Context) error {
			calls.Add(1)
			cancel()
			<-leaderCtx.Done()
			return leaderCtx.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLeaderElected error = %v, want context cancellation", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("reconcile calls = %d, want 1", calls.Load())
	}
}

func TestRunLeaderElectedValidatesDurations(t *testing.T) {
	client := kubernetesfake.NewClientset()
	err := RunLeaderElected(context.Background(), client.CoordinationV1(), LeaderOptions{
		Namespace: "system", Name: "controller", Identity: "host-one",
		LeaseDuration: time.Second, RenewDeadline: time.Second, RetryPeriod: 100 * time.Millisecond,
		Reconcile: func(context.Context) error { return nil },
	})
	if err == nil {
		t.Fatal("invalid leader election durations were accepted")
	}
}

func TestRunLeaderElectedStopsWhenReconcileExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := kubernetesfake.NewClientset()
	err := RunLeaderElected(ctx, client.CoordinationV1(), LeaderOptions{
		Namespace: "system", Name: "controller", Identity: "host-one",
		LeaseDuration: time.Second, RenewDeadline: 700 * time.Millisecond, RetryPeriod: 100 * time.Millisecond,
		Reconcile: func(context.Context) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
		t.Fatalf("unexpected reconcile exit error = %v", err)
	}
}
