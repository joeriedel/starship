package main

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	errBusy := errors.New("busy")
	calls := 0
	err := retry(time.Second, func(e error) bool { return e == errBusy }, func() error {
		calls++
		if calls <= 3 {
			return errBusy
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("want success on the 4th call, got err=%v calls=%d", err, calls)
	}
}

func TestRetryReturnsNonTransientAtOnce(t *testing.T) {
	calls := 0
	err := retry(time.Second, func(error) bool { return false }, func() error {
		calls++
		return fs.ErrNotExist
	})
	if !errors.Is(err, fs.ErrNotExist) || calls != 1 {
		t.Fatalf("want one call and ErrNotExist, got err=%v calls=%d", err, calls)
	}
}

func TestRetryGivesUpWithLastError(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retry(50*time.Millisecond, func(error) bool { return true }, func() error {
		calls++
		return fs.ErrPermission
	})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("want the last error, got %v", err)
	}
	if calls < 2 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("gave up too early: calls=%d after %v", calls, time.Since(start))
	}
}
