package main

import (
	"context"
	"errors"

	"signal-headless/internal/paths"
)

var errTODO = errors.New("not implemented yet")

func runDaemon(ctx context.Context, o *options, p paths.Paths) error { return errTODO }
func runStatus(ctx context.Context, o *options, p paths.Paths) error { return errTODO }
func runSend(ctx context.Context, o *options, p paths.Paths) error   { return errTODO }
func runShell(ctx context.Context, o *options, p paths.Paths) error  { return errTODO }
