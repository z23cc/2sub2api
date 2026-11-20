//go:build unit

package adminaccess

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedUsageAndWritePermissionCatalog(t *testing.T) {
	catalog := PermissionCatalog()
	for _, want := range []Permission{
		{ID: "accounts.usage.read", Label: "读取账户用量与额度", Category: "账户", Scope: "account"},
		{ID: "proxies.write", Label: "创建、修改、删除与测试代理 IP", Category: "代理", Scope: "global"},
		{ID: "groups.write", Label: "创建分组与修改分组设置", Category: "分组", Scope: "global"},
	} {
		t.Run(want.ID, func(t *testing.T) {
			var found []Permission
			for _, permission := range catalog {
				if permission.ID == want.ID {
					found = append(found, permission)
				}
			}
			require.Equal(t, []Permission{want}, found)
		})
	}
}

func TestScopedUsageAndWritePermissionRoutes(t *testing.T) {
	for _, tc := range []struct {
		permission string
		global     bool
		adjacent   []string
		routes     []string
	}{
		{"accounts.usage.read", false, []string{"accounts.read"}, []string{"POST /accounts/today-stats/batch", "GET /accounts/:id/stats", "POST /accounts/usage/batch"}},
		{"proxies.write", true, []string{"proxies.read"}, []string{"POST /proxies", "PUT /proxies/:id", "DELETE /proxies/:id", "POST /proxies/:id/test"}},
		{"groups.write", true, []string{"groups.read"}, []string{"POST /groups", "PUT /groups/:id"}},
	} {
		t.Run(tc.permission, func(t *testing.T) {
			resourcePolicy := Policy{Rules: []Rule{{Permissions: []string{tc.permission}, GroupIDs: []int64{7}, AccountTypes: []string{"oauth"}}}}
			globalPolicy := Policy{GlobalPermissions: []string{tc.permission}}
			correct, wrong := resourcePolicy, globalPolicy
			adjacent := Policy{Rules: []Rule{{Permissions: tc.adjacent, GroupIDs: []int64{7}, AccountTypes: []string{"oauth"}}}}
			if tc.global {
				correct, wrong = globalPolicy, resourcePolicy
				adjacent = Policy{GlobalPermissions: tc.adjacent}
			}
			require.NoError(t, ValidatePolicy(correct))
			require.ErrorContains(t, ValidatePolicy(wrong), tc.permission)

			for _, route := range tc.routes {
				t.Run(route, func(t *testing.T) {
					method, suffix, _ := strings.Cut(route, " ")
					path := "/api/v1/admin" + suffix
					require.True(t, RouteAllowed(correct, method, path))
					require.False(t, RouteAllowed(Policy{}, method, path))
					require.False(t, RouteAllowed(wrong, method, path))
					require.False(t, RouteAllowed(adjacent, method, path))
					require.False(t, RouteAllowed(correct, http.MethodPatch, path))
					require.False(t, RouteAllowed(correct, method, path+"/unregistered"))
				})
			}
		})
	}

	t.Run("模型权限只开放既有修改入口", func(t *testing.T) {
		policy := Policy{Rules: []Rule{{Permissions: []string{"groups.models.write"}, GroupIDs: []int64{7}}}}
		require.False(t, RouteAllowed(policy, http.MethodPost, "/api/v1/admin/groups"))
		// PUT 入口由两项权限共享，处理器会限制只持有模型权限的请求字段。
		require.True(t, RouteAllowed(policy, http.MethodPut, "/api/v1/admin/groups/:id"))
	})
}
