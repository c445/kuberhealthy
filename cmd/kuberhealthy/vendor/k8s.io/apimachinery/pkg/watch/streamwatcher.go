/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// Decoder allows StreamWatcher to watch any stream for which a Decoder can be written.
type Decoder interface {
	// Decode should return the type of event, the decoded object, or an error.
	// An error will cause StreamWatcher to call Close(). Decode should block until
	// it has data or an error occurs.
	Decode() (action EventType, object runtime.Object, err error)

	// Close should close the underlying io.Reader, signalling to the source of
	// the stream that it is no longer being watched. Close() must cause any
	// outstanding call to Decode() to return with an error of some sort.
	Close()
}

// Reporter hides the details of how an error is turned into a runtime.Object for
// reporting on a watch stream since this package may not import a higher level report.
type Reporter interface {
	// AsObject must convert err into a valid runtime.Object for the watch stream.
	AsObject(err error) runtime.Object
}

// StreamWatcher turns any stream for which you can write a Decoder interface
// into a watch.Interface.
type StreamWatcher struct {
	sync.Mutex
	source   Decoder
	reporter Reporter
	result   chan Event
	stopped  bool
}

// NewStreamWatcher creates a StreamWatcher from the given decoder.
func NewStreamWatcher(d Decoder, r Reporter) *StreamWatcher {
	sw := &StreamWatcher{
		source:   d,
		reporter: r,
		// It's easy for a consumer to add buffering via an extra
		// goroutine/channel, but impossible for them to remove it,
		// so nonbuffered is better.
		result: make(chan Event),
	}
	cl := NewCodeLocation(5)
	go sw.receive(cl.FullStackTrace)
	return sw
}

// ResultChan implements Interface.
func (sw *StreamWatcher) ResultChan() <-chan Event {
	return sw.result
}

// Stop implements Interface.
func (sw *StreamWatcher) Stop() {
	// Call Close() exactly once by locking and setting a flag.
	sw.Lock()
	defer sw.Unlock()
	if !sw.stopped {
		sw.stopped = true
		sw.source.Close()
	}
}

// stopping returns true if Stop() was called previously.
func (sw *StreamWatcher) stopping() bool {
	sw.Lock()
	defer sw.Unlock()
	return sw.stopped
}

// receive reads result from the decoder in a loop and sends down the result channel.
func (sw *StreamWatcher) receive(s string) {
	fmt.Printf("GoRoutine: Started watcher %q: %s\n", getGID(), s)
	defer close(sw.result)
	defer sw.Stop()
	defer utilruntime.HandleCrash()
	for {
		action, obj, err := sw.source.Decode()
		if err != nil {
			// Ignore expected error.
			if sw.stopping() {
				return
			}
			switch err {
			case io.EOF:
				// watch closed normally
			case io.ErrUnexpectedEOF:
				klog.V(1).Infof("Unexpected EOF during watch stream event decoding: %v", err)
			default:
				if net.IsProbableEOF(err) {
					klog.V(5).Infof("Unable to decode an event from the watch stream: %v", err)
				} else {
					sw.result <- Event{
						Type:   Error,
						Object: sw.reporter.AsObject(fmt.Errorf("unable to decode an event from the watch stream: %v", err)),
					}
				}
			}
			return
		}

		sw.result <- Event{
			Type:   action,
			Object: obj,
		}
	}
}

func getGID() uint64 {
	b := make([]byte, 64)
	b = b[:goruntime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}


func NewCodeLocation(skip int) CodeLocation {
	_, file, line, _ := goruntime.Caller(skip + 1)
	stackTrace := PruneStack(string(debug.Stack()), skip+1)
	return CodeLocation{FileName: file, LineNumber: line, FullStackTrace: stackTrace}
}

// PruneStack removes references to functions that are internal to Ginkgo
// and the Go runtime from a stack string and a certain number of stack entries
// at the beginning of the stack. The stack string has the format
// as returned by runtime/debug.Stack. The leading goroutine information is
// optional and always removed if present. Beware that runtime/debug.Stack
// adds itself as first entry, so typically skip must be >= 1 to remove that
// entry.
func PruneStack(fullStackTrace string, skip int) string {
	stack := strings.Split(fullStackTrace, "\n")
	// Ensure that the even entries are the method names and the
	// the odd entries the source code information.
	if len(stack) > 0 && strings.HasPrefix(stack[0], "goroutine ") {
		// Ignore "goroutine 29 [running]:" line.
		stack = stack[1:]
	}
	// The "+1" is for skipping over the initial entry, which is
	// runtime/debug.Stack() itself.
	if len(stack) > 2*(skip+1) {
		stack = stack[2*(skip+1):]
	}
	prunedStack := []string{}
	re := regexp.MustCompile(`\/ginkgo\/|\/pkg\/testing\/|\/pkg\/runtime\/`)
	for i := 0; i < len(stack)/2; i++ {
		// We filter out based on the source code file name.
		if !re.Match([]byte(stack[i*2+1])) {
			prunedStack = append(prunedStack, stack[i*2])
			prunedStack = append(prunedStack, stack[i*2+1])
		}
	}
	return strings.Join(prunedStack, "\n")
}

type CodeLocation struct {
	FileName       string
	LineNumber     int
	FullStackTrace string
}

func (codeLocation CodeLocation) String() string {
	return fmt.Sprintf("%s:%d", codeLocation.FileName, codeLocation.LineNumber)
}

