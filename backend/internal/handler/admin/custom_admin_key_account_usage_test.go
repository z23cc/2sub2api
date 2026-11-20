//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scopedUsageAdminService struct {
	service.AdminService
	accounts map[int64]*service.Account
}

func (s *scopedUsageAdminService) GetAccountsByIDs(_ context.Context, ids []int64) ([]*service.Account, error) {
	var accounts []*service.Account
	for _, id := range ids {
		if account := s.accounts[id]; account != nil {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (s *scopedUsageAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if account := s.accounts[id]; account != nil {
		return account, nil
	}
	return nil, service.ErrAccountNotFound
}

type scopedUsageLogRepo struct {
	service.UsageLogRepository
	batches  [][]int64
	statsIDs []int64
}

func (r *scopedUsageLogRepo) GetAccountWindowStatsBatch(_ context.Context, ids []int64, _ time.Time) (map[int64]*usagestats.AccountStats, error) {
	r.batches = append(r.batches, append([]int64(nil), ids...))
	return map[int64]*usagestats.AccountStats{}, nil
}

func (*scopedUsageLogRepo) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
	return &usagestats.AccountStats{}, nil
}

func (r *scopedUsageLogRepo) GetAccountUsageStats(_ context.Context, id int64, _, _ time.Time) (*usagestats.AccountUsageStatsResponse, error) {
	r.statsIDs = append(r.statsIDs, id)
	return &usagestats.AccountUsageStatsResponse{}, nil
}

type scopedUsageAccountRepo struct {
	service.AccountRepository
	accounts *scopedUsageAdminService
	batches  [][]int64
}

func (r *scopedUsageAccountRepo) GetByIDs(ctx context.Context, ids []int64) ([]*service.Account, error) {
	r.batches = append(r.batches, append([]int64(nil), ids...))
	return r.accounts.GetAccountsByIDs(ctx, ids)
}

func TestScopedAccountUsageDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		base       = "/api/v1/admin/accounts"
		allowedID  = int64(810201)
		deniedID   = int64(810202)
		todayBatch = "/today-stats/batch"
		usageBatch = "/usage/batch"
		mixedIDs   = `{"account_ids":[810201,810202,810203]}`
	)
	accounts := &scopedUsageAdminService{accounts: map[int64]*service.Account{
		allowedID: {ID: allowedID, GroupIDs: []int64{7}, Type: service.AccountTypeOAuth, Platform: service.PlatformOpenAI, Extra: map[string]any{
			"codex_5h_used_percent": 15, "codex_7d_used_percent": 30,
		}},
		deniedID: {ID: deniedID, GroupIDs: []int64{8}, Type: service.AccountTypeOAuth, Platform: service.PlatformOpenAI},
	}}
	newRouter := func(permission string) (*gin.Engine, *scopedUsageLogRepo, *scopedUsageAccountRepo) {
		logs := &scopedUsageLogRepo{}
		repo := &scopedUsageAccountRepo{accounts: accounts}
		h := &AccountHandler{adminService: accounts, accountUsageService: service.NewAccountUsageService(repo, logs, nil, nil, nil, nil, nil, nil, nil, nil, nil)}
		policy := adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{permission}, GroupIDs: []int64{7}, AccountTypes: []string{service.AccountTypeOAuth}}}}
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{Policy: policy}))
		}, h.ScopedAdminKeyAccounts)
		fallback := func(c *gin.Context) { c.Status(http.StatusTeapot) }
		r.POST(base+todayBatch, fallback)
		r.POST(base+usageBatch, fallback)
		r.GET(base+"/:id/stats", fallback)
		return r, logs, repo
	}
	call := func(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("今日统计只查询并返回有权账号", func(t *testing.T) {
		cacheKey := buildAccountTodayStatsBatchCacheKey([]int64{allowedID})
		t.Cleanup(func() {
			accountTodayStatsBatchCache.mu.Lock()
			defer accountTodayStatsBatchCache.mu.Unlock()
			delete(accountTodayStatsBatchCache.items, cacheKey)
		})
		r, logs, repo := newRouter("accounts.usage.read")
		w := call(r, http.MethodPost, todayBatch, mixedIDs)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, [][]int64{{allowedID}, {allowedID}}, logs.batches)
		require.Empty(t, repo.batches)
		var payload struct {
			Data struct {
				Stats map[int64]*service.WindowStats `json:"stats"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
		require.Len(t, payload.Data.Stats, 1)
		require.NotNil(t, payload.Data.Stats[allowedID])
	})

	t.Run("批量额度只查询并返回有权账号且保留force", func(t *testing.T) {
		for _, force := range []bool{false, true} {
			r, logs, repo := newRouter("accounts.usage.read")
			body := strings.TrimSuffix(mixedIDs, "}") + `,"force":` + strconv.FormatBool(force) + "}"
			w := call(r, http.MethodPost, usageBatch, body)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, [][]int64{{allowedID}}, repo.batches)
			require.Empty(t, logs.batches)
			var payload struct {
				Data struct {
					Usage  map[int64]*service.UsageInfo `json:"usage"`
					Errors map[int64]string             `json:"errors"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
			require.Empty(t, payload.Data.Errors)
			require.Len(t, payload.Data.Usage, 1)
			usage := payload.Data.Usage[allowedID]
			require.NotNil(t, usage)
			require.Equal(t, "passive", usage.Source)
			// 快照已完整。只有 force=true 才尝试刷新，账号没有 token，失败发生在发起网络请求前。
			if force {
				require.Equal(t, "quota_refresh_failed", usage.ErrorCode)
			} else {
				require.Empty(t, usage.ErrorCode)
			}
		}
	})

	for _, batch := range []struct{ path, empty string }{
		{todayBatch, `{"stats":{}}`},
		{usageBatch, `{"usage":{},"errors":{}}`},
	} {
		for _, tc := range []struct {
			name, permission, body string
			status                 int
		}{
			{"空数组", "accounts.usage.read", `{"account_ids":[]}`, 200},
			{"全部无权或不存在", "accounts.usage.read", `{"account_ids":[810202,810203]}`, 200},
			{"缺少账号ID", "accounts.usage.read", `{"force":true}`, 400},
			{"缺少用量权限", "accounts.read", mixedIDs, 403},
		} {
			t.Run(batch.path+"/"+tc.name, func(t *testing.T) {
				r, logs, repo := newRouter(tc.permission)
				w := call(r, http.MethodPost, batch.path, tc.body)
				require.Equal(t, tc.status, w.Code, w.Body.String())
				require.Empty(t, logs.batches)
				require.Empty(t, repo.batches)
				if tc.status == 200 {
					var payload struct {
						Data json.RawMessage `json:"data"`
					}
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
					require.JSONEq(t, batch.empty, string(payload.Data))
				}
			})
		}
	}

	t.Run("单账号用量区分路由权限并隐藏账号是否存在", func(t *testing.T) {
		r, logs, repo := newRouter("accounts.usage.read")
		denied := call(r, http.MethodGet, "/810202/stats", "")
		missing := call(r, http.MethodGet, "/810203/stats", "")
		require.Equal(t, 404, denied.Code, denied.Body.String())
		require.Equal(t, 404, missing.Code, missing.Body.String())
		require.JSONEq(t, denied.Body.String(), missing.Body.String())
		require.Empty(t, logs.statsIDs)

		allowed := call(r, http.MethodGet, "/810201/stats", "")
		require.Equal(t, 200, allowed.Code, allowed.Body.String())
		require.Equal(t, []int64{allowedID}, logs.statsIDs)
		require.Empty(t, repo.batches)

		r, logs, _ = newRouter("accounts.read")
		w := call(r, http.MethodGet, "/810201/stats", "")
		require.Equal(t, 403, w.Code, w.Body.String())
		require.Empty(t, logs.statsIDs)
	})
}
