//
// DISCLAIMER
//
// Copyright 2017 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
)

var (
	maskAny     = errors.WithStack
	stdoutMutex sync.Mutex
)

type SubProcess struct {
	cmd    *exec.Cmd
	dump   bool
	stderr io.ReadCloser
	stdout io.ReadCloser
	stdin  io.WriteCloser

	mutex       sync.Mutex
	output      bytes.Buffer
	expressions map[*regexp.Regexp]chan struct{}
}

// NewSubProcess creates a new process with given name and arguments.
// The process is not yet starter.
func NewSubProcess(name string, arg ...string) (*SubProcess, error) {
	sp := &SubProcess{
		expressions: make(map[*regexp.Regexp]chan struct{}),
		dump:        true,
	}
	sp.cmd = exec.Command(name, arg...)
	var err error
	sp.stderr, err = sp.cmd.StderrPipe()
	if err != nil {
		return nil, maskAny(err)
	}
	sp.stdout, err = sp.cmd.StdoutPipe()
	if err != nil {
		return nil, maskAny(err)
	}
	sp.stdin, err = sp.cmd.StdinPipe()
	if err != nil {
		return nil, maskAny(err)
	}
	return sp, nil
}

// Start the process
func (sp *SubProcess) Start() error {
	slurp := func(rd io.ReadCloser) {
		byteBuf := make([]byte, 512)
		for {
			n, err := rd.Read(byteBuf)
			sp.writeOutput(byteBuf[:n])
			sp.matchExpressions()
			if err != nil {
				break
			}
		}
	}
	if err := sp.cmd.Start(); err != nil {
		return maskAny(err)
	}
	go slurp(sp.stderr)
	go slurp(sp.stdout)
	return nil
}

// Close terminates the process.
func (sp *SubProcess) Close() error {
	if p := sp.cmd.Process; p != nil {
		p.Signal(syscall.SIGTERM)
		p.Wait()
	}
	return nil
}

// Kill terminates the process the hard way.
func (sp *SubProcess) Kill() error {
	if p := sp.cmd.Process; p != nil {
		p.Signal(syscall.SIGKILL)
		p.Wait()
	}
	return nil
}

// SendIntr sends a SIGINT to the process.
func (sp *SubProcess) SendIntr() error {
	if p := sp.cmd.Process; p != nil {
		p.Signal(syscall.SIGINT)
	}
	return nil
}

// WaitTimeout waits for the process to terminate.
// Kill the process after the given timeout.
func (sp *SubProcess) WaitTimeout(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(timeout):
			// Kill the process
			sp.Close()
		case <-done:
			// Just return
		}
	}()
	err := sp.cmd.Wait()
	close(done)
	return maskAny(err)
}

// Wait waits for the process to terminate.
func (sp *SubProcess) Wait() error {
	if err := sp.cmd.Wait(); err != nil {
		return maskAny(err)
	}
	return nil
}

// ExpectTimeout waits for the output of the process to match the given expression, or until a timeout occurs.
// If a match on the given expression is found, the process output is discard until the end of the match and
// nil is returned, otherwise a timeout error is returned.
// If the given context is cancelled, nil is returned.
func (sp *SubProcess) ExpectTimeout(ctx context.Context, timeout time.Duration, re *regexp.Regexp, id string) error {
	found := make(chan struct{})

	sp.mutex.Lock()
	sp.expressions[re] = found
	sp.mutex.Unlock()

	sp.matchExpressions()

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(timeout):
		// Return timeout error
		var output []byte
		sp.mutex.Lock()
		output = sp.output.Bytes()
		sp.mutex.Unlock()

		stdoutMutex.Lock()
		defer stdoutMutex.Unlock()
		fmt.Printf("Timeout while waiting for '%s' in %s\nOutput so far:\n", re, id)
		os.Stdout.Write(output)
		return errors.New("Timeout")
	case <-found:
		// Success
		return nil
	}
}

func (sp *SubProcess) writeOutput(data []byte) {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	sp.output.Write(data)
}

func (sp *SubProcess) matchExpressions() {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	for re, found := range sp.expressions {
		loc := re.FindIndex(sp.output.Bytes())
		if loc == nil {
			// No match
			continue
		}
		// Found a match, remove everything until the end of the match
		n := loc[1]
		sp.output.Next(n)
		close(found)
		// Remove from map
		delete(sp.expressions, re)
	}
}
