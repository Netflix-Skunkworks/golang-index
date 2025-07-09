package internal

import (
	"math/rand"
	"time"
)

// The following is copied from https://github.com/googleapis/gax-go/blob/7025124cca5102d146b0054710b8ea1183fc602f/v2/call_option.go.
// A little copying is better than a little dependency. -Rob Pike https://www.youtube.com/watch?v=PAAkCSZUG1c&t=568s

// Backoff implements backoff logic for retries. The configuration for retries
// is described in https://google.aip.dev/client-libraries/4221. The current
// retry limit starts at Initial and increases by a factor of Multiplier every
// retry, but is capped at Max. The actual wait time between retries is a
// random value between 1ns and the current retry limit. The purpose of this
// random jitter is explained in
// https://www.awsarchitectureblog.com/2015/03/backoff.html.
//
// Note: MaxNumRetries / RPCDeadline is specifically not provided. These should
// be built on top of Backoff.
type Backoff struct {
	// Initial is the initial value of the retry period, defaults to 1 second.
	Initial time.Duration

	// Max is the maximum value of the retry period, defaults to 30 seconds.
	Max time.Duration

	// Multiplier is the factor by which the retry period increases.
	// It should be greater than 1 and defaults to 2.
	Multiplier float64

	// cur is the current retry period.
	cur time.Duration
}

// Pause returns the next time.Duration that the caller should use to backoff.
func (bo *Backoff) Pause() time.Duration {
	if bo.Initial == 0 {
		bo.Initial = time.Second
	}
	if bo.cur == 0 {
		bo.cur = bo.Initial
	}
	if bo.Max == 0 {
		bo.Max = 30 * time.Second
	}
	if bo.Multiplier < 1 {
		bo.Multiplier = 2
	}
	// Select a duration between 1ns and the current max. It might seem
	// counterintuitive to have so much jitter, but
	// https://www.awsarchitectureblog.com/2015/03/backoff.html argues that
	// that is the best strategy.
	d := time.Duration(1 + rand.Int63n(int64(bo.cur)))
	bo.cur = min(time.Duration(float64(bo.cur)*bo.Multiplier), bo.Max)
	return d
}
