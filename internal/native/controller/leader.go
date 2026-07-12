package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type LeaderOptions struct {
	Namespace     string
	Name          string
	Identity      string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
	Reconcile     func(context.Context) error
	Logf          func(string, ...any)
}

func RunLeaderElected(ctx context.Context, client coordinationclient.CoordinationV1Interface, options LeaderOptions) error {
	if client == nil || options.Namespace == "" || options.Name == "" || options.Identity == "" || options.Reconcile == nil {
		return fmt.Errorf("coordination client, leader lock identity, and reconcile function are required")
	}
	if options.LeaseDuration <= options.RenewDeadline || options.RenewDeadline <= options.RetryPeriod || options.RetryPeriod <= 0 {
		return fmt.Errorf("leader election durations must satisfy lease duration > renew deadline > retry period > 0")
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Namespace: options.Namespace, Name: options.Name},
		Client:    client,
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: options.Identity,
		},
	}
	electionCtx, cancelElection := context.WithCancel(ctx)
	defer cancelElection()
	reconcileErrors := make(chan error, 1)
	var lostLeadership atomic.Bool
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   options.LeaseDuration,
		RenewDeadline:   options.RenewDeadline,
		RetryPeriod:     options.RetryPeriod,
		ReleaseOnCancel: true,
		Name:            options.Name,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				if options.Logf != nil {
					options.Logf("acquired controller leadership as %s", options.Identity)
				}
				err := options.Reconcile(leaderCtx)
				if err == nil && leaderCtx.Err() == nil {
					err = fmt.Errorf("leader reconcile loop exited unexpectedly")
				}
				select {
				case reconcileErrors <- err:
				default:
				}
				cancelElection()
			},
			OnStoppedLeading: func() {
				if ctx.Err() == nil {
					lostLeadership.Store(true)
				}
			},
			OnNewLeader: func(identity string) {
				if identity != options.Identity && options.Logf != nil {
					options.Logf("controller leader is %s", identity)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("configure controller leader election: %w", err)
	}
	elector.Run(electionCtx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case err := <-reconcileErrors:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("controller reconcile loop: %w", err)
		}
	default:
	}
	if lostLeadership.Load() {
		return fmt.Errorf("controller lost leadership")
	}
	return nil
}
