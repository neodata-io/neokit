package jobs_test

import (
	"context"
	"fmt"
	"time"

	"github.com/neodata-io/neokit/jobs"
)

func ExampleJob() {
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{}, 1)

	job := jobs.Job{
		Name:       "refresh-prices",
		Every:      15 * time.Minute,
		Timeout:    30 * time.Second,
		RunAtStart: true,
		Do: func(ctx context.Context) error {
			fmt.Println("refreshed")
			ran <- struct{}{}
			return nil
		},
	}

	// In a service: job.Start(a.Context()), which app drains at shutdown.
	done := make(chan struct{})
	go func() { job.Run(ctx); close(done) }()

	<-ran
	cancel()
	<-done
	// Output: refreshed
}

func ExampleDaily() {
	var ctx context.Context // a.Context() in a service

	// Fires at 08:00 local time every day, DST included.
	jobs.Daily{
		Name:    "morning-digest",
		Hour:    8,
		Timeout: time.Minute,
		Do:      func(ctx context.Context) error { return nil },
	}.Start(ctx)
}
