package postgres_test

import "errors"

func is(err, target error) bool { return errors.Is(err, target) }
