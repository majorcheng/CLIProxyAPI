package handlers

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	log "github.com/sirupsen/logrus"
)

type countingFlusher struct {
	count int
}

func (f *countingFlusher) Flush() {
	f.count++
}

func TestForwardStreamFlushesDoneAndBatchesChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/stream", nil)

	data := make(chan []byte, 2)
	data <- []byte("one")
	data <- []byte("two")
	close(data)

	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	flusher := &countingFlusher{}
	cancelCalls := 0
	handler := &BaseAPIHandler{}
	handler.ForwardStream(c, flusher, func(error) {
		cancelCalls++
	}, data, errs, StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			_, _ = c.Writer.Write(chunk)
		},
		WriteDone: func() {
			_, _ = c.Writer.Write([]byte("[DONE]"))
		},
	})

	if got := recorder.Body.String(); got != "onetwo[DONE]" {
		t.Fatalf("body = %q, want %q", got, "onetwo[DONE]")
	}
	if flusher.count != 1 {
		t.Fatalf("flush count = %d, want %d", flusher.count, 1)
	}
	if cancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want %d", cancelCalls, 1)
	}
}

func TestForwardStreamMarksAPIResponseTimestampOnFirstChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/stream", nil)

	data := make(chan []byte, 1)
	data <- []byte("one")
	close(data)

	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	before := time.Now()
	handler := &BaseAPIHandler{}
	handler.ForwardStream(c, &countingFlusher{}, func(error) {}, data, errs, StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			_, _ = c.Writer.Write(chunk)
		},
	})

	rawTimestamp, exists := c.Get(apiResponseTimestampKey)
	if !exists {
		t.Fatalf("%s missing after first stream chunk", apiResponseTimestampKey)
	}
	timestamp, ok := rawTimestamp.(time.Time)
	if !ok {
		t.Fatalf("%s type = %T, want time.Time", apiResponseTimestampKey, rawTimestamp)
	}
	if timestamp.Before(before) {
		t.Fatalf("%s = %s, want >= %s", apiResponseTimestampKey, timestamp, before)
	}
}

func TestForwardStreamFlushesKeepAliveAndTerminalError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/stream", nil)

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)

	flusher := &countingFlusher{}
	cancelErrCh := make(chan error, 1)
	handler := &BaseAPIHandler{}

	go func() {
		time.Sleep(10 * time.Millisecond)
		errs <- &interfaces.ErrorMessage{Error: errors.New("boom")}
		close(errs)
	}()

	keepAliveInterval := 1 * time.Millisecond
	handler.ForwardStream(c, flusher, func(err error) {
		cancelErrCh <- err
	}, data, errs, StreamForwardOptions{
		KeepAliveInterval: &keepAliveInterval,
		WriteKeepAlive: func() {
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			_, _ = c.Writer.Write([]byte("error:" + errMsg.Error.Error()))
		},
	})

	if flusher.count < 2 {
		t.Fatalf("flush count = %d, want at least %d", flusher.count, 2)
	}
	if got := recorder.Body.String(); len(got) < len("error:boom") || got[len(got)-len("error:boom"):] != "error:boom" {
		t.Fatalf("body = %q, want terminal error suffix", got)
	}
	if err := <-cancelErrCh; err == nil || err.Error() != "boom" {
		t.Fatalf("cancel err = %v, want boom", err)
	}
}

func TestForwardStream_DebugLogsTerminalErrorAfterFirstChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/stream", nil)
	logging.SetGinRequestID(c, "reqstream")

	var logBuf bytes.Buffer
	restore := swapTestLogger(&logBuf, log.DebugLevel)
	defer restore()

	data := make(chan []byte, 1)
	data <- []byte("partial")

	errs := make(chan *interfaces.ErrorMessage, 1)

	flusher := &countingFlusher{}
	cancelErrCh := make(chan error, 1)
	handler := &BaseAPIHandler{}

	go func() {
		time.Sleep(5 * time.Millisecond)
		errs <- &interfaces.ErrorMessage{StatusCode: 502, Error: errors.New("upstream boom")}
		close(errs)
	}()

	handler.ForwardStream(c, flusher, func(err error) {
		cancelErrCh <- err
	}, data, errs, StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			_, _ = c.Writer.Write(chunk)
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			_, _ = c.Writer.Write([]byte("error:" + errMsg.Error.Error()))
		},
	})

	if got := recorder.Body.String(); got != "partialerror:upstream boom" {
		t.Fatalf("body = %q, want %q", got, "partialerror:upstream boom")
	}
	if err := <-cancelErrCh; err == nil || err.Error() != "upstream boom" {
		t.Fatalf("cancel err = %v, want upstream boom", err)
	}

	output := logBuf.String()
	if !strings.Contains(output, "streaming terminal error, status: 502, message: upstream boom") {
		t.Fatalf("debug log missing streaming error summary, got %q", output)
	}
	if !strings.Contains(output, "request_id=reqstream") {
		t.Fatalf("debug log missing request id, got %q", output)
	}
}
