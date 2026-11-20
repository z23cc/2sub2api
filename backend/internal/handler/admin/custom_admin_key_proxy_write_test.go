//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scopedProxyWriteService struct {
	service.AdminService
	proxy     service.Proxy
	creates   []*service.CreateProxyInput
	updateIDs []int64
	deleteIDs []int64
	testIDs   []int64
}

func (s *scopedProxyWriteService) CreateProxy(_ context.Context, input *service.CreateProxyInput) (*service.Proxy, error) {
	s.creates = append(s.creates, input)
	return &s.proxy, nil
}

func (s *scopedProxyWriteService) UpdateProxy(_ context.Context, id int64, _ *service.UpdateProxyInput) (*service.Proxy, error) {
	s.updateIDs = append(s.updateIDs, id)
	return &s.proxy, nil
}

func (s *scopedProxyWriteService) DeleteProxy(_ context.Context, id int64) error {
	s.deleteIDs = append(s.deleteIDs, id)
	return nil
}

func (s *scopedProxyWriteService) TestProxy(_ context.Context, id int64) (*service.ProxyTestResult, error) {
	s.testIDs = append(s.testIDs, id)
	return &service.ProxyTestResult{Success: true}, nil
}

func TestScopedProxyWriteRoutesAndRedaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := service.DefaultIdempotencyCoordinator()
	repo := newMemoryIdempotencyRepoStub()
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(repo, service.DefaultIdempotencyConfig()))
	t.Cleanup(func() { service.SetDefaultIdempotencyCoordinator(previous) })

	const base = "/api/v1/admin/proxies"
	const idempotencyKey = "scoped-proxy-write-fixture"
	api := &scopedProxyWriteService{proxy: service.Proxy{
		ID: 17, Host: "192.0.2.17", Username: "proxy-user-fixture", Password: "proxy-password-fixture",
	}}
	h := NewProxyHandler(api)
	newRouter := func(principal *adminaccess.Principal) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if principal != nil {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
				if !adminaccess.RouteAllowed(principal.Policy, c.Request.Method, c.FullPath()) {
					c.AbortWithStatus(http.StatusForbidden)
				}
			}
		})
		r.POST(base, h.Create)
		r.PUT(base+"/:id", h.Update)
		r.DELETE(base+"/:id", h.Delete)
		r.POST(base+"/:id/test", h.Test)
		return r
	}
	call := func(r *gin.Engine, method, suffix, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, base+suffix, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotencyKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	requireRedacted := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"host":"192.0.2.17"`)
		for _, secret := range []string{"username", "password", api.proxy.Username, api.proxy.Password} {
			require.NotContains(t, w.Body.String(), secret)
		}
	}

	r := newRouter(&adminaccess.Principal{
		KeyID: "proxy-test-key", Policy: adminaccess.Policy{GlobalPermissions: []string{"proxies.write"}},
	})
	t.Run("创建仍校验参数并脱敏响应及回放", func(t *testing.T) {
		invalid := call(r, http.MethodPost, "", `{}`)
		require.Equal(t, 400, invalid.Code, invalid.Body.String())
		require.Empty(t, api.creates)

		const body = `{"name":"测试代理","protocol":"http","host":"192.0.2.17","port":8080,"username":"proxy-user-fixture","password":"proxy-password-fixture"}`
		first := call(r, http.MethodPost, "", body)
		requireRedacted(t, first)
		require.Empty(t, first.Header().Get("X-Idempotency-Replayed"))
		replay := call(r, http.MethodPost, "", body)
		requireRedacted(t, replay)
		require.Equal(t, "true", replay.Header().Get("X-Idempotency-Replayed"))
		require.Len(t, api.creates, 1)
		require.Equal(t, api.proxy.Username, api.creates[0].Username)
		require.Equal(t, api.proxy.Password, api.creates[0].Password)
		ctx := context.Background()
		keyHash := service.HashIdempotencyKey(idempotencyKey)
		record, err := repo.GetByScopeAndKeyHash(ctx, "admin.proxies.create:scoped:proxy-test-key", keyHash)
		require.NoError(t, err)
		require.NotNil(t, record)
		fullRecord, err := repo.GetByScopeAndKeyHash(ctx, "admin.proxies.create", keyHash)
		require.NoError(t, err)
		require.Nil(t, fullRecord)
	})
	t.Run("修改脱敏且完整管理员保留原响应", func(t *testing.T) {
		requireRedacted(t, call(r, http.MethodPut, "/17", `{"name":"更新代理"}`))
		require.Equal(t, []int64{17}, api.updateIDs)
		w := call(newRouter(nil), http.MethodPut, "/17", `{"name":"更新代理"}`)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"username":"proxy-user-fixture"`)
		require.Contains(t, w.Body.String(), `"password":"proxy-password-fixture"`)
	})
	t.Run("删除和连通性测试调用原服务", func(t *testing.T) {
		deleted := call(r, http.MethodDelete, "/17", "")
		require.Equal(t, 200, deleted.Code, deleted.Body.String())
		require.Equal(t, []int64{17}, api.deleteIDs)
		tested := call(r, http.MethodPost, "/17/test", "")
		require.Equal(t, 200, tested.Code, tested.Body.String())
		require.Contains(t, tested.Body.String(), `"success":true`)
		require.Equal(t, []int64{17}, api.testIDs)
	})
}

func TestScopedProxyCreateIdempotencyScopeIsolatedByKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies", nil)
	require.Equal(t, "admin.proxies.create", scopedAdminProxyCreateScope(c))

	for _, key := range []string{"key-a", "key-b"} {
		c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: key}))
		require.Equal(t, "admin.proxies.create:scoped:"+key, scopedAdminProxyCreateScope(c))
	}
}
