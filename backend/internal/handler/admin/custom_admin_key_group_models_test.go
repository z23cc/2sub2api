//go:build unit

package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scopedGroupWriteService struct {
	service.AdminService
	writes []int64
	config service.GroupModelAllowlist
}

func (*scopedGroupWriteService) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	return &service.Group{ID: id, Platform: service.PlatformOpenAI}, nil
}

func (s *scopedGroupWriteService) ScopedAdminUpdateGroupModels(_ context.Context, id int64, config service.GroupModelAllowlist) (*service.Group, error) {
	s.writes = append(s.writes, id)
	s.config = config
	return &service.Group{ID: id, Platform: service.PlatformOpenAI, ModelAllowlist: config}, nil
}

func TestScopedGroupWriteDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	full := adminaccess.Policy{GlobalPermissions: []string{"groups.write"}}
	models := adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"groups.models.write"}, GroupIDs: []int64{7}}}}
	const fullBody = `{"name":"新分组","rate_multiplier":2}`
	const modelsBody = `{"model_allowlist":{"enabled":true,"models":["gpt-test"]}}`
	for _, tc := range []struct {
		name, method, path, body string
		policy                   adminaccess.Policy
		status                   int
		fullHandler, modelWrite  bool
	}{
		{"完整更新", http.MethodPut, "/7", fullBody, full, 204, true, false},
		{"创建分组", http.MethodPost, "", fullBody, full, 204, true, false},
		{"模型权限拒绝完整字段", http.MethodPut, "/7", fullBody, models, 400, false, false},
		{"模型权限允许单独修改白名单", http.MethodPut, "/7", modelsBody, models, 200, false, true},
		{"模型权限拒绝创建", http.MethodPost, "", fullBody, models, 403, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &scopedGroupWriteService{}
			h := NewGroupHandler(api, nil, nil)
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{Policy: tc.policy}))
				if !adminaccess.RouteAllowed(tc.policy, c.Request.Method, c.FullPath()) {
					c.AbortWithStatus(http.StatusForbidden)
				}
			}, h.ScopedAdminKeyGroupModels)
			var reached bool
			fullHandler := func(c *gin.Context) {
				reached = true
				raw, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.JSONEq(t, fullBody, string(raw))
				c.Status(http.StatusNoContent)
			}
			const base = "/api/v1/admin/groups"
			r.POST(base, fullHandler)
			r.PUT(base+"/:id", fullHandler)
			req := httptest.NewRequest(tc.method, base+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, tc.fullHandler, reached)
			if tc.modelWrite {
				require.Equal(t, []int64{7}, api.writes)
				require.True(t, api.config.Enabled)
				require.Equal(t, []string{"gpt-test"}, api.config.Models)
			} else {
				require.Empty(t, api.writes)
			}
		})
	}
}
