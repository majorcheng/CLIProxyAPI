package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestRecordAPIRequestStoresReasoningEffortWhenRequestLogDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	recordAPIRequest(ctx, nil, upstreamRequestLog{
		Provider: "codex",
		Body:     []byte(`{"reasoning":{"effort":"xhigh"}}`),
	})

	raw, exists := ginCtx.Get(internalusage.RequestReasoningEffortContextKey)
	if !exists {
		t.Fatalf("expected reasoning effort to be stored in gin context")
	}
	if got, _ := raw.(string); got != "xhigh" {
		t.Fatalf("reasoning effort = %q, want %q", got, "xhigh")
	}
}
