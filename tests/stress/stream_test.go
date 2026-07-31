package stress

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/gatewayapi"
	"github.com/akz142857/Heimdall/internal/openaiapi"
)

const concurrentStreams = 1000

type streamService struct {
	started chan struct{}
	release <-chan struct{}
	active  atomic.Int64
}

func (*streamService) Chat(context.Context, string, openaiapi.ChatCompletionRequest) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}

func (s *streamService) ChatStream(ctx context.Context, _ string, request openaiapi.ChatCompletionRequest, emit func(openaiapi.ChatCompletionResponse) error) error {
	s.active.Add(1)
	defer s.active.Add(-1)
	if err := emit(openaiapi.ChatCompletionResponse{
		ID: "stress", Object: "chat.completion.chunk", Model: request.Model,
		Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent("started")}}},
	}); err != nil {
		return err
	}
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return emit(openaiapi.ChatCompletionResponse{
		ID: "stress", Object: "chat.completion.chunk", Model: request.Model,
		Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Content: openaiapi.TextContent("finished")}}},
	})
}

func (*streamService) Embeddings(context.Context, string, openaiapi.EmbeddingRequest) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}

func TestThousandConcurrentSSEConnectionsCleanup(t *testing.T) {
	if os.Getenv("HEIMDALL_STRESS") != "1" {
		t.Skip("set HEIMDALL_STRESS=1 for the release stress gate")
	}
	release := make(chan struct{})
	service := &streamService{started: make(chan struct{}, concurrentStreams), release: release}
	handler, err := gatewayapi.NewWithOptions(service, gatewayapi.Options{
		MaxRequestBytes: 1024, RouteTimeout: 30 * time.Second,
		StreamTimeout: 30 * time.Second, WriteTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(handler.ChatCompletions))
	defer server.Close()
	transport := &http.Transport{
		Proxy: nil, MaxConnsPerHost: concurrentStreams,
		MaxIdleConns: concurrentStreams, MaxIdleConnsPerHost: concurrentStreams,
		IdleConnTimeout: time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 40 * time.Second}
	defer transport.CloseIdleConnections()
	readResponses := make(chan struct{})

	runtime.GC()
	before := processSample()
	if before.FDs < 0 {
		t.Fatal("cannot count process file descriptors; install lsof or run on a host with /proc/self/fd")
	}
	body, _ := json.Marshal(openaiapi.ChatCompletionRequest{
		Model: "chat", Stream: true,
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
	})
	errs := make(chan error, concurrentStreams)
	var wait sync.WaitGroup
	wait.Add(concurrentStreams)
	for index := 0; index < concurrentStreams; index++ {
		go func() {
			defer wait.Done()
			request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			request.Header.Set("Authorization", "Bearer gw_stress")
			request.Header.Set("Content-Type", "application/json")
			response, err := client.Do(request)
			if err != nil {
				errs <- err
				return
			}
			// Hold every client after response headers and the first SSE event. This
			// exercises cleanup with 1,000 connected clients that are not reading.
			<-readResponses
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusOK {
				errs <- &statusError{status: response.StatusCode}
			} else if readErr != nil {
				errs <- readErr
			} else if closeErr != nil {
				errs <- closeErr
			} else {
				errs <- nil
			}
		}()
	}
	deadline := time.After(20 * time.Second)
	for index := 0; index < concurrentStreams; index++ {
		select {
		case <-service.started:
		case <-deadline:
			t.Fatalf("only %d/%d streams became active", index, concurrentStreams)
		}
	}
	peak := processSample()
	if service.active.Load() != concurrentStreams {
		t.Fatalf("active streams=%d", service.active.Load())
	}
	close(release)
	close(readResponses)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	transport.CloseIdleConnections()
	var after sample
	for attempt := 0; attempt < 50; attempt++ {
		runtime.GC()
		after = processSample()
		if service.active.Load() == 0 && after.Goroutines <= before.Goroutines+40 && after.FDs <= before.FDs+40 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if service.active.Load() != 0 || after.Goroutines > before.Goroutines+40 || after.FDs > before.FDs+40 {
		t.Fatalf("resources did not clean up: before=%+v peak=%+v after=%+v active=%d", before, peak, after, service.active.Load())
	}
	peakHeapPerConnection := float64(peak.HeapBytes-before.HeapBytes) / concurrentStreams
	peakRSSPerConnection := float64(peak.RSSBytes-before.RSSBytes) / concurrentStreams
	t.Logf("streams=%d before=%+v peak=%+v after=%+v peak_heap_per_connection=%.0f peak_rss_per_connection=%.0f cpu_seconds=%.3f",
		concurrentStreams, before, peak, after, peakHeapPerConnection, peakRSSPerConnection, after.CPUSeconds-before.CPUSeconds)
}

type statusError struct{ status int }

func (e *statusError) Error() string { return http.StatusText(e.status) }

type sample struct {
	HeapBytes  uint64
	RSSBytes   uint64
	CPUSeconds float64
	Goroutines int
	FDs        int
}

func processSample() sample {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	rss, cpu := processResources()
	return sample{HeapBytes: memory.HeapAlloc, RSSBytes: rss, CPUSeconds: cpu, Goroutines: runtime.NumGoroutine(), FDs: openFDs()}
}

func processResources() (uint64, float64) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0
	}
	rss := uint64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1_000_000
	return rss, cpu
}

func openFDs() int {
	for _, path := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(path)
		if err == nil {
			return len(entries)
		}
	}
	output, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(os.Getpid()), "-Fn").Output()
	if err == nil {
		count := 0
		for _, line := range bytes.Split(output, []byte{'\n'}) {
			if len(line) > 1 && line[0] == 'f' && line[1] >= '0' && line[1] <= '9' {
				count++
			}
		}
		if count > 0 {
			return count
		}
	}
	return -1
}
