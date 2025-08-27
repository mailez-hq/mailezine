package stackhttp

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySuccessFirstAttempt(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 3, time.Millisecond, func() (bool, error) {
		calls++
		return false, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("expected success on first attempt, got calls=%d err=%v", calls, err)
	}
}

func TestRetryDefinitiveErrorNotRetried(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 3, time.Millisecond, func() (bool, error) {
		calls++
		return false, errors.New("definitive")
	})
	if err == nil || calls != 1 {
		t.Fatalf("definitive error must not be retried: calls=%d", calls)
	}
}

func TestRetryExhaustsAttempts(t *testing.T) {
	want := errors.New("503")
	calls := 0
	err := Retry(context.Background(), 3, time.Millisecond, func() (bool, error) {
		calls++
		return true, want
	})
	if !errors.Is(err, want) || calls != 3 {
		t.Fatalf("want last=503 after 3 attempts, got calls=%d err=%v", calls, err)
	}
}

func TestRetryNilOnSuccess(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 3, time.Millisecond, func() (bool, error) {
		calls++
		if calls < 2 {
			return true, errors.New("transient")
		}
		return false, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want success on attempt 2, got calls=%d err=%v", calls, err)
	}
}

func TestRetryContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Retry(ctx, 5, time.Hour, func() (bool, error) {
		calls++
		cancel()
		return true, errors.New("503")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("want ctx cancel during backoff, got calls=%d err=%v", calls, err)
	}
}
