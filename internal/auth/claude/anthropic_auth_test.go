package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_429BlocksImmediateReplay(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
					Header:     http.Header{"Retry-After": []string{"60"}},
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected 429 refresh error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected status 429 in error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt after 429, got %d", got)
	}

	_, err = auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected immediate blocked refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected blocked retry to avoid a second refresh call, got %d attempts", got)
	}
	if blockedUntil := claudeRefreshBlockedUntil(auth.refreshSingleflightKey("dummy_refresh_token")); !blockedUntil.After(time.Now()) {
		t.Fatalf("expected blocked-until timestamp to be set, got %v", blockedUntil)
	}
}

func TestRefreshTokens_DeduplicatesConcurrentRefresh(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				once.Do(func() { close(started) })
				<-release
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(`{
						"access_token":"new-access",
						"refresh_token":"new-refresh",
						"token_type":"Bearer",
						"expires_in":3600,
						"account":{"email_address":"shared@example.com"}
					}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}

	results := make(chan *ClaudeTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func() {
		td, err := auth.RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}

	go runRefresh()
	go runRefresh()

	<-started
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		td := <-results
		if td == nil || td.AccessToken != "new-access" {
			t.Fatalf("expected refreshed access token, got %#v", td)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
}

func TestRefreshTokens_DoesNotShareFirstCallerContext(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				startedOnce.Do(func() { close(started) })
				select {
				case <-req.Context().Done():
					return nil, req.Context().Err()
				case <-release:
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(strings.NewReader(`{
							"access_token":"shared-access",
							"refresh_token":"shared-refresh",
							"token_type":"Bearer",
							"expires_in":3600,
							"account":{"email_address":"shared@example.com"}
						}`)),
						Header:  make(http.Header),
						Request: req,
					}, nil
				}
			}),
		},
	}

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelFirst()
	firstErr := make(chan error, 1)
	go func() {
		_, err := auth.RefreshTokens(firstCtx, "shared-context-refresh-token")
		firstErr <- err
	}()

	<-started
	secondResult := make(chan *ClaudeTokenData, 1)
	secondErr := make(chan error, 1)
	go func() {
		td, err := auth.RefreshTokens(context.Background(), "shared-context-refresh-token")
		secondResult <- td
		secondErr <- err
	}()

	select {
	case err := <-firstErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first refresh error = %v, want context deadline exceeded", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected first refresh to return on its own deadline")
	}

	select {
	case err := <-secondErr:
		t.Fatalf("second refresh completed before upstream release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	closeRelease()
	if err := <-secondErr; err != nil {
		t.Fatalf("expected second refresh to succeed, got %v", err)
	}
	if td := <-secondResult; td == nil || td.AccessToken != "shared-access" {
		t.Fatalf("unexpected second token data: %#v", td)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
}

func TestRefreshTokensHonorsContextDeadline(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	done := make(chan struct{})
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				defer close(done)
				select {
				case <-req.Context().Done():
					return nil, req.Context().Err()
				case <-time.After(80 * time.Millisecond):
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(strings.NewReader(`{
							"access_token":"late-access",
							"refresh_token":"late-refresh",
							"token_type":"Bearer",
							"expires_in":3600,
							"account":{"email_address":"late@example.com"}
						}`)),
						Header:  make(http.Header),
						Request: req,
					}, nil
				}
			}),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := auth.RefreshTokens(ctx, "deadline-refresh-token")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected refresh request goroutine to finish")
	}
}

func TestRefreshTokens_DoesNotDeduplicateDifferentProxyKeys(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	newAuth := func(proxyKey, accessToken string) *ClaudeAuth {
		return &ClaudeAuth{
			refreshProxyKey: proxyKey,
			httpClient: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					atomic.AddInt32(&calls, 1)
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(strings.NewReader(`{
							"access_token":"` + accessToken + `",
							"refresh_token":"new-refresh",
							"token_type":"Bearer",
							"expires_in":3600,
							"account":{"email_address":"proxy@example.com"}
						}`)),
						Header:  make(http.Header),
						Request: req,
					}, nil
				}),
			},
		}
	}

	errs := make(chan error, 2)
	results := make(chan *ClaudeTokenData, 2)
	go func() {
		td, err := newAuth("socks5://proxy-a.example:1080", "access-a").RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}()
	go func() {
		td, err := newAuth("socks5://proxy-b.example:1080", "access-b").RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}()

	deadline := time.After(200 * time.Millisecond)
	for atomic.LoadInt32(&calls) < 2 {
		select {
		case <-deadline:
			t.Fatalf("expected different proxy keys to start two upstream refresh calls, got %d", atomic.LoadInt32(&calls))
		case <-time.After(5 * time.Millisecond):
		}
	}
	closeRelease()

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		if td := <-results; td == nil || !strings.HasPrefix(td.AccessToken, "access-") {
			t.Fatalf("unexpected token data: %#v", td)
		}
	}
}
