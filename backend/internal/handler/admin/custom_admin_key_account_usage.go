package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func scopedAdminAccountResource(a *service.Account) adminaccess.Account {
	return adminaccess.Account{ID: a.ID, GroupIDs: a.GroupIDs, Type: a.Type, Platform: a.Platform}
}

// scopedAdminBatchUsage 处理两个用量批量接口：先剔除当前密钥无权读取用量的账户，
// 再把改写后的请求体交给原处理函数。不存在与无权的账户同样被剔除，响应里看不出区别。
func (h *AccountHandler) scopedAdminBatchUsage(c *gin.Context, principal *adminaccess.Principal, usage bool) {
	if !adminaccess.RouteAllowed(principal.Policy, c.Request.Method, c.FullPath()) {
		response.Error(c, 403, "用量读取未授权")
		return
	}
	var req BatchUsageRequest
	if c.ShouldBindJSON(&req) != nil {
		response.Error(c, 400, "请求格式无效")
		return
	}
	ids := normalizeInt64IDList(req.AccountIDs)
	allowed := make([]int64, 0, len(ids))
	if len(ids) > 0 {
		accounts, err := h.adminService.GetAccountsByIDs(c.Request.Context(), ids)
		if err != nil {
			response.Error(c, 503, "账户查询失败")
			return
		}
		for _, a := range accounts {
			if a != nil && principal.Policy.AllowsAccount("accounts.usage.read", scopedAdminAccountResource(a)) {
				allowed = append(allowed, a.ID)
			}
		}
	}
	body := gin.H{"account_ids": allowed}
	if usage {
		body["force"] = req.Force
	}
	raw, _ := json.Marshal(body)
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	c.Request.ContentLength = int64(len(raw))
	if usage {
		h.GetBatchUsage(c)
	} else {
		h.GetBatchTodayStats(c)
	}
}

// scopedAdminAccountStats 处理单账户用量统计；无权与不存在一律返回 404。
func (h *AccountHandler) scopedAdminAccountStats(c *gin.Context, principal *adminaccess.Principal) {
	if !adminaccess.RouteAllowed(principal.Policy, c.Request.Method, c.FullPath()) {
		response.Error(c, 403, "用量读取未授权")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	a, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil || a == nil || !principal.Policy.AllowsAccount("accounts.usage.read", scopedAdminAccountResource(a)) {
		response.Error(c, 404, "账户不存在")
		return
	}
	h.GetStats(c)
}
