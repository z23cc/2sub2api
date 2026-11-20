//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestScopedCodexBaseRPMValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"0", true}, {"120", true}, {"1000000", true},
		{"-1", false}, {"1000001", false}, {"1.5", false}, {`"10"`, false}, {"true", false}, {"null", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			var req scopedCodexSessionImportRequest
			require.NoError(t, json.Unmarshal([]byte(`{"group_ids":[7],"extra":{"base_rpm":`+tc.value+`}}`), &req))
			err := validateScopedCodexRequest(req)
			if tc.valid {
				require.IsType(t, float64(0), req.Extra["base_rpm"])
				require.NoError(t, err)
			} else {
				require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
				require.Equal(t, "SCOPED_CODEX_FIELD", infraerrors.Reason(err))
			}
		})
	}
}

func TestScopedCodexImportSkipExistingAndBaseRPM(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := buildCodexAccessToken(t, "workspace", "member", time.Now().Add(time.Hour))
	existing := service.Account{ID: 91, Credentials: map[string]any{"access_token": token}}
	update, noUpdate := true, false
	for _, tc := range []struct {
		name           string
		accounts       []service.Account
		updateExisting *bool
	}{
		{"匹配已有账号", []service.Account{existing}, nil},
		{"同时开启更新时优先跳过", []service.Account{existing}, &update},
		{"关闭更新仍查找并跳过", []service.Account{existing}, &noUpdate},
		{"无匹配账号时照常创建并保留base_rpm", nil, &noUpdate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := &scopedCodexMemoryService{codexImportMemoryAdminService: newCodexImportMemoryAdminService(tc.accounts)}
			h := &AccountHandler{adminService: memory}
			principal := &adminaccess.Principal{KeyID: "codex-options", Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
				Permissions:  []string{"accounts.create", "accounts.credentials", "accounts.schedule"},
				GroupIDs:     []int64{7},
				AccountTypes: []string{service.AccountTypeOAuth},
			}}}}
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
			}, h.ScopedAdminKeyAccounts)
			const path = "/api/v1/admin/accounts/import/codex-session"
			r.POST(path, func(c *gin.Context) { t.Fatal("不能回退到完整管理员导入") })
			body, err := json.Marshal(CodexSessionImportRequest{
				Content: token, GroupIDs: []int64{7}, Extra: map[string]any{"base_rpm": 120},
				SkipExisting: true, UpdateExisting: tc.updateExisting,
			})
			require.NoError(t, err)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
			require.Equal(t, 200, w.Code, w.Body.String())

			var payload struct {
				Data CodexSessionImportResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
			result := payload.Data
			require.Len(t, memory.lookups, 1)
			require.Equal(t, token, memory.lookups[0].AccessToken)
			require.Empty(t, memory.updatedAccounts)
			require.Zero(t, result.Updated)
			require.Zero(t, result.Failed)
			require.Len(t, result.Items, 1)
			if len(tc.accounts) > 0 {
				require.Empty(t, memory.createdAccounts)
				require.Equal(t, 1, result.Skipped)
				require.Zero(t, result.Created)
				require.Equal(t, "skipped", result.Items[0].Action)
				require.Equal(t, existing.ID, result.Items[0].AccountID)
			} else {
				require.Len(t, memory.createdAccounts, 1)
				require.Equal(t, float64(120), memory.createdAccounts[0].Extra["base_rpm"])
				require.Equal(t, 1, result.Created)
				require.Zero(t, result.Skipped)
				require.Equal(t, "created", result.Items[0].Action)
			}
		})
	}
}
