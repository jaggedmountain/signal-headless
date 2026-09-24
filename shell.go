package main

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"

	"signal-headless/internal/paths"
	"signal-headless/internal/tui"
)

func runShell(ctx context.Context, o *options, p paths.Paths) error {
	c, err := connect(ctx, o, p, true)
	if err != nil {
		return err
	}
	defer c.Close()
	m := tui.New(c, tui.Options{Bell: os.Getenv("SIGNAL_HEADLESS_BELL") != "0"})
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err == tea.ErrProgramKilled && ctx.Err() != nil {
		return nil
	}
	return err
}
