// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package check provides validation helpers for ygnmi queries.

These helpers wrap the .Lookup() and .Watch()/.Await() methods of Ondatra's
ygnmi structs, eliminating a great deal of boilerplate and allowing
table-driven testing of paths of different types.

# Overview

This package provides a set of functions for creating Validator objects, each
of which represents a check to be performed against some gNMI path. A Validator
can then be invoked in several different ways to actually perform the check.
Typical usage looks like this:

	// Create a table of Validators.
	validators := []check.Validator{
		check.Equal(ocpath.Some().Path().State(), someValue),
		check.Present(ocpath.Some().OtherPath().Config()),
		check.NotEqual(ocpath.Another().Path().State(), anotherValue),
	}
	// Check each one and report any failures.
	for _, vd:= range validators {
		t.Run(vd.Path(), func(t *testing.T) {
			if err := vd.Check(gnmiClient); err != nil {
				// err will already look like e.g. "some/path/state: got 12, want 0"
				// so no further formatting is necessary here.
				t.Error(err)
			}
		})
	}

# Validator functions

The most generic validation function is

	check.Validate(query, validationFn func(Value[T]) error)

This Validator validates by running validationFn on the query's value and
returning any resulting error.

check also provides a number of shorthands for common cases:

  - check.Equal(query, want) checks that the query's value is want.
  - check.NotEqual(query, wantNot) checks that the query has any value other
    than wantNot.
  - check.Present(query) checks that the query has any value at all.
  - check.NotPresent(query) checks that the query is unset.
  - check.EqualOrNil(query, want) checks that the query's value is want OR
    that the query is unset.
  - check.Predicate[T](query PathStruct[T], wantMsg string, predicate func(T)
    bool) checks that the value at query is present and satisfies the given
    predicate function; wantMsg is used in the resulting error if it fails.

These helpers all have prewritten validation functions that return sensible
errors of the form "<path>: <got>, <want>", such as:

	/system/some/path: got no value, want 12
	/system/hostname: got "wrongname", want "node1" or nil
	/some/other/path: got 100, want no value

# Validating a Validator

Given a Validator, there are several ways to test its condition:

  - vd.Check(client) executes the query immediately, tests the result, and
    returns any error generated by the validation function.
  - vd.Await(ctx, client) will watch the specified path and return nil as soon
    as the validation passes; if this never happens, it will continue blocking
    until the context expires or is canceled.
  - vd.AwaitUntil(deadline, client) is almost the same as creating a context
    with the given deadline and calling Await(), except that if the deadline is
    in the past it will call Check() instead.
  - vd.AwaitFor(timeout, client) is AwaitUntil(time.Now().Add(timeout), client)

# Accommodating latency

There will often be some small latency between when a configuration variable is
set via gNMI and when the corresponding operational state reflects the change.
As a result, it's common to want to test several values with the expectation
that all of them will be correct within some short window of time. The
preferred way to do this is:

	deadline := time.Now().Add(time.Second())
	for _, vd:= range[]check.Validator {
		check.Equal(root.Some().Path(), someValue),
		check.Present(root.Some().OtherPath()),
		...
		check.NotEqual(root.Another().Path(), anotherValue),
	} {
		t.Run(vd.Path(), func(t *testing.T) {
			if err := vd.AwaitUntil(deadline, client); err != nil {
				t.Error(err)
			}
		})
	}

The above code expects that every validation will pass within one second of the
start of the block. Don't use AwaitFor this, or else on a failing device the
test could wait one second *per validator* instead of total.

Note that the above is also preferable to

	ctx, _ := context.WithTimeout(context.Background(), time.Second())
	for _, vd:= range[]check.Validator {
		...
	} {
		t.Run(vd.Path(), func(t *testing.T) {
			if err := vd.Await(ctx, client); err != nil {
				t.Error(err)
			}
		})
	}

This is because if the first validator times out, the other validators won't
run because Await(ctx, client) aborts if the ctx has already expired, whereas
AwaitUntil and AwaitFor will both be equivalent to Check if given a 0 or
negative timeout or a deadline in the past.

# Error Messages

The error messages generated by failing checks will include the path, the value
at that path, and a description of what the validator wanted, e.g.

	some/path: got 12, want 19
*/
package check

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"github.com/openconfig/ygot/ygot"
	"github.com/openconfig/ygnmi/ygnmi"
)

// FormatPath formats a PathStruct for display. On unresolvable or otherwise
// broken path structs, it will return a string indicating that this is an
// unprintable path.
func FormatPath(path ygnmi.PathStruct) string {
	if path == nil {
		return "<nil path>"
	}
	gpath, _, err := ygnmi.ResolvePath(path)
	if err != nil {
		return fmt.Sprintf("<Unprintable path: %v>", err)
	}
	str, err := ygot.PathToString(gpath)
	if err != nil {
		return fmt.Sprintf("<Unprintable path: %v>", err)
	}
	return str
}

// FormatValue formats v's value, or returns "no value" if v isn't present.
func FormatValue[T any](v *ygnmi.Value[T]) string {
	if v == nil {
		return "nil"
	}
	val, present := v.Val()
	if !present {
		return "no value"
	}
	return fmt.Sprintf("%#v", val)
}

// FormatRelativePath formats a path relative to base.
func FormatRelativePath(base, path ygnmi.PathStruct) string {
	baseStr := FormatPath(base)
	pathStr := FormatPath(path)
	relStr, err := filepath.Rel(baseStr, pathStr)
	if err != nil {
		return pathStr
	}
	return relStr
}

// Validator is an interface representing a validation operation that could
// have latency. The Check method fetches a value and validates it immediately;
// the Await* methods watch the query's value waiting for a value that passes
// validation.
// Note that AwaitFor and AwaitUntil are equivalent to Check if you pass in a
// negative duration or deadline in the past.
type Validator interface {
	Check(*ygnmi.Client) error
	Await(context.Context, *ygnmi.Client) error
	AwaitFor(time.Duration, *ygnmi.Client) error
	AwaitUntil(time.Time, *ygnmi.Client) error
	Path() string
	RelPath(ygnmi.PathStruct) string
}

// validationError represents an error that occurred during validation. It will
// have .validationErr set if the error is that the validation failed, and
// .failureCause set if something else went wrong, such as a network error.
// Check errors will generally only have one of these two set, but Await errors
// will always have a failureCause set, most commonly the DeadlineExceeded that
// ended the await, and will frequently also have a validationErr (the error
// generated by the most recent call to the validation function).
type validationError[T any] struct {
	query ygnmi.SingletonQuery[T]
	// validationErr is the error returned by the validation function.
	validationErr error
	// failureCause is the error that triggered this error. This will be nil if
	// the validation error is the only thing that went wrong (e.g. on a Check),
	// but will be set if something else, like a network error, happened.
	failureCause error
}

func (f *validationError[T]) qStr() string {
	return FormatPath(f.query.PathStruct())
}

func (f *validationError[T]) Error() string {
	if isTimeout(f.failureCause) {
		// in the special (common) case where Await failed because of a timeout,
		// print the validation error.
		if f.validationErr != nil {
			return fmt.Sprintf("%s: %v (deadline exceeded)", f.qStr(), f.validationErr)
		}
		return fmt.Sprintf("%s: deadline exceeded before any values were fetched", f.qStr())
	}
	// If we had a network etc. failure, print that (ignoring validation errors)
	if f.failureCause != nil {
		return fmt.Sprintf("%s: %v", f.qStr(), f.failureCause)
	}
	// Otherwise print the validation error
	if f.validationErr != nil {
		return fmt.Sprintf("%s: %v", f.qStr(), f.validationErr)
	}
	// In theory this shouldn't happen.
	return fmt.Sprintf("%s: unknown error", f.qStr())
}

var _ error = (*validationError[any])(nil)

// isTimeout returns true if and only if err is a status.DeadlineExceeded.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var stat interface {
		GRPCStatus() *status.Status
	}
	if errors.As(err, &stat) {
		return stat.GRPCStatus().Code() == codes.DeadlineExceeded
	}
	return false
}

// validation is the common implementation of Validator.
type validation[T any] struct {
	query        ygnmi.SingletonQuery[T]
	validationFn func(*ygnmi.Value[T]) error
}

var _ Validator = (*validation[any])(nil)

// Path returns a string representation of the path being validated.
func (vd *validation[T]) Path() string {
	return FormatPath(vd.query.PathStruct())
}

// RelPath returns a string representation of the path being validated,
// relative to some base.
func (vd *validation[T]) RelPath(base ygnmi.PathStruct) string {
	return FormatRelativePath(base, vd.query.PathStruct())
}

// Check tests the validation condition immediately and returns an error if it
// fails.
func (vd *validation[T]) Check(client *ygnmi.Client) error {
	lastVal, err := ygnmi.Lookup(context.Background(), client, vd.query)
	if err != nil {
		return &validationError[T]{
			query:        vd.query,
			failureCause: err}
	}
	if err := vd.validationFn(lastVal); err != nil {
		return &validationError[T]{
			query:         vd.query,
			validationErr: err,
		}
	}
	return nil
}

// Await waits for the validation condition to run without error; it returns
// nil when it does so or an error if anything goes wrong. Await will always
// try to fetch at least one value; canceling the passed-in context only stops
// it from waiting for correct values if it has already received an incorrect
// one.
func (vd *validation[T]) Await(ctx context.Context, client *ygnmi.Client) error {
	// Do a plain check first, regardless of timeouts
	var checkErr *validationError[T]
	err := vd.Check(client)
	if err == nil || !errors.As(err, &checkErr) || checkErr.failureCause != nil {
		// Either validation succeeded, or we couldn't fetch the value
		return err
	}
	// If we get here, we fetched the value just fine but it was invalid, so we
	// Watch until the context expires or we receive a valid value.
	lastInvalid := checkErr.validationErr
	watcher := ygnmi.Watch(ctx, client, vd.query, func(v *ygnmi.Value[T]) error {
		if lastInvalid = vd.validationFn(v); lastInvalid != nil {
			return ygnmi.Continue
		}
		return nil
	})
	_, err = watcher.Await()
	if err != nil {
		failed := &validationError[T]{
			query:        vd.query,
			failureCause: err,
		}
		if lastInvalid != nil {
			failed.validationErr = lastInvalid
		}
		return failed
	}
	return nil
}

// AwaitFor calls Await with a context with deadline now + timeout. If timeout
// is <= 0, this is equivalent to Check().
func (vd *validation[T]) AwaitFor(timeout time.Duration, client *ygnmi.Client) error {
	if timeout <= 0 {
		return vd.Check(client)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return vd.Await(ctx, client)
}

// AwaitUntil calls Await with a context with the given deadline. If deadline
// is in the past, this is equivalent to Check().
func (vd *validation[T]) AwaitUntil(deadline time.Time, client *ygnmi.Client) error {
	if deadline.Before(time.Now()) {
		return vd.Check(client)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return vd.Await(ctx, client)
}

// Validate expects validationFn to return no error on the query's value.
func Validate[T any, QT ygnmi.SingletonQuery[T]](query QT, validationFn func(*ygnmi.Value[T]) error) Validator {
	return &validation[T]{query, validationFn}
}

// Predicate expects that the query has a value and the given predicate returns
// true on that value. The wantMsg will be included in any validation failure,
// e.g. if wantMsg is "want a multiple of 4", the error might read:
//
//	"/some/path: got 13, want a multiple of 4".
func Predicate[T any, QT ygnmi.SingletonQuery[T]](query QT, wantMsg string, predicate func(T) bool) Validator {
	return Validate(query, func(vgot *ygnmi.Value[T]) error {
		got, present := vgot.Val()
		if !present || !predicate(got) {
			return fmt.Errorf("got %s, %s", FormatValue(vgot), wantMsg)
		}
		return nil
	})
}

// Equal expects the query's value to be want.
func Equal[T any, QT ygnmi.SingletonQuery[T]](query QT, want T) Validator {
	return Predicate(query, fmt.Sprintf("want %#v", want), func(got T) bool {
		return reflect.DeepEqual(got, want)
	})
}

// NotEqual expects the query to have a value other than wantNot.
func NotEqual[T any, QT ygnmi.SingletonQuery[T]](query QT, wantNot T) Validator {
	return Predicate(query, fmt.Sprintf("want anything but %#v", wantNot), func(got T) bool {
		return !reflect.DeepEqual(got, wantNot)
	})
}

// EqualOrNil expects the query to be unset or have value want.
func EqualOrNil[T any, QT ygnmi.SingletonQuery[T]](query QT, want T) Validator {
	return Validate(query, func(vgot *ygnmi.Value[T]) error {
		got, present := vgot.Val()
		if present && !reflect.DeepEqual(got, want) {
			return fmt.Errorf("got %s, want %#v or no value", FormatValue(vgot), want)
		}
		return nil
	})
}

// Present expects the query to have any value.
func Present[T any, QT ygnmi.SingletonQuery[T]](query QT) Validator {
	return Predicate(query, "want any value", func(T) bool {
		return true
	})
}

// NotPresent expects the query to not have a value set.
func NotPresent[T any, QT ygnmi.SingletonQuery[T]](query QT) Validator {
	return Validate(query, func(vgot *ygnmi.Value[T]) error {
		if vgot.IsPresent() {
			return fmt.Errorf("got %s, want no value", FormatValue(vgot))
		}
		return nil
	})
}

// UnorderedEqual function is used to compare slices of type T in unordered way.
func UnorderedEqual[T any, QT ygnmi.SingletonQuery[[]T]](query QT, want []T, less func(a, b T) bool) Validator {
	return Predicate(query, fmt.Sprintf("want %#v", want), func(got []T) bool {
		// Sort slices to compare them in unorderd way.
		return cmp.Equal(got, want, cmpopts.SortSlices(less))
	})
}
