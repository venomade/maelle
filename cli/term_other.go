//go:build !linux

package main

import (
	"errors"
	"os"
)

var errNoRaw = errors.New("raw terminal mode is only supported on linux")

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

type rawState struct{}

func makeRaw(f *os.File) (*rawState, error) { return nil, errNoRaw }
func (r *rawState) restore(f *os.File)      {}

func detectWidth() int {
	if !isCharDevice(os.Stdout) {
		return 0
	}
	return 80
}

func watchResize(c chan<- os.Signal)     {}
func stopWatchResize(c chan<- os.Signal) {}
